package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/guestctl"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// TaskChannel dispatches a headless agent run over the control channel (AT1).
// *guestctl.Manager satisfies it. The run is async — completion arrives as
// agent.done and updates the task row directly; the HTTP layer only dispatches.
type TaskChannel interface {
	// RunAgent dispatches a headless run; sessionID, when non-empty, resumes a
	// prior agent session for a multi-turn follow-up (AT4).
	RunAgent(ctx context.Context, machineID, taskID, repoPath, prompt, provider, sessionID string) error
	// CancelAgent signals a running task to stop (AT3). The terminal `canceled`
	// status arrives asynchronously via agent.done.
	CancelAgent(ctx context.Context, machineID, taskID string) error
}

// createTaskRequest is the body of POST /api/machines/{id}/tasks.
type createTaskRequest struct {
	Prompt   string `json:"prompt"`
	Provider string `json:"provider"`
	Project  string `json:"project"`
}

// taskIDResponse is the 202 body of POST /api/machines/{id}/tasks.
type taskIDResponse struct {
	TaskID string `json:"task_id"`
}

// taskView is one agent task in the GET responses. Result fields are populated
// only once the run is terminal.
type taskView struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Provider      string          `json:"provider"`
	Project       string          `json:"project"`
	SessionID     string          `json:"agent_session_id,omitempty"`
	Usage         json.RawMessage `json:"usage,omitempty"`
	ResultSummary string          `json:"result_summary,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     string          `json:"created_at"`
	StartedAt     string          `json:"started_at,omitempty"`
	EndedAt       string          `json:"ended_at,omitempty"`
}

type tasksResponse struct {
	Tasks []taskView `json:"tasks"`
}

// handleCreateTask dispatches a headless agent run in a project (AT1). The agent
// produces a dirty working tree and stops — it never commits (that is the
// separate GR flow). Returns 202 + task_id; completion is observed by polling
// GET .../tasks/{tid}.
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" || req.Provider == "" || req.Project == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}

	machineID, ok := s.resolveWorktreeMachine(w, r, user)
	if !ok {
		return
	}

	// Provider must be enabled, headless-capable (Claude Code only on this lane),
	// and have a stored key.
	prov, err := s.Providers.Get(r.Context(), req.Provider)
	if err != nil || !prov.Enabled {
		writeError(w, http.StatusNotFound, "unknown_provider")
		return
	}
	if prov.LaunchCommand != "claude" {
		writeError(w, http.StatusBadRequest, "provider_not_headless")
		return
	}
	uid := uuidString(user.ID)
	if !s.providerKeySet(uid, req.Provider) {
		writeError(w, http.StatusConflict, "no_provider_key")
		return
	}

	repoPath, code := s.resolveProject(r.Context(), machineID, req.Project)
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}

	// Push the user's provider key into the guest before the run (idempotent).
	if s.Injector != nil {
		if err := s.Injector.Inject(r.Context(), uid, machineID); err != nil {
			writeError(w, http.StatusBadGateway, "injection_failed")
			return
		}
	}

	mid, _ := machine.ParseUUID(machineID)
	uidU, _ := machine.ParseUUID(uid)
	task, err := s.Queries.InsertAgentTask(r.Context(), store.InsertAgentTaskParams{
		MachineID: mid, UserID: uidU, Provider: req.Provider, Project: req.Project, Prompt: req.Prompt,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "task_create_failed")
		return
	}
	taskID := machine.UUIDString(task.ID)

	if err := s.TaskChannel.RunAgent(r.Context(), machineID, taskID, repoPath, req.Prompt, req.Provider, ""); err != nil {
		// Dispatch failed — the run never started; close the task as failed.
		_ = s.Queries.FinishAgentTask(r.Context(), store.FinishAgentTaskParams{
			ID: task.ID, Status: "failed", Usage: []byte("{}"), Error: "dispatch failed",
		})
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusBadGateway, "dispatch_failed")
		return
	}
	_ = s.Queries.MarkAgentTaskRunning(r.Context(), task.ID)

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionAgentTaskRun,
		Target:   req.Project,
		Metadata: map[string]any{"task_id": taskID, "provider": req.Provider},
	})
	writeJSON(w, http.StatusAccepted, taskIDResponse{TaskID: taskID})
}

// handleListTasks returns a machine's agent tasks, newest first. It does not
// require the machine to be running (a finished task is still readable).
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	rows, err := s.Queries.ListAgentTasksByMachine(r.Context(), mc.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed")
		return
	}
	views := make([]taskView, 0, len(rows))
	for _, t := range rows {
		views = append(views, toTaskView(t))
	}
	writeJSON(w, http.StatusOK, tasksResponse{Tasks: views})
}

// handleGetTask returns one task's status + result. The task must belong to the
// {id} machine (and thus the caller).
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	tid, err := machine.ParseUUID(r.PathValue("tid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	task, err := s.Queries.GetAgentTask(r.Context(), tid)
	if err != nil || machine.UUIDString(task.MachineID) != machine.UUIDString(mc.ID) {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	writeJSON(w, http.StatusOK, toTaskView(task))
}

// handleCancelTask requests cancellation of a running task (AT3). It dispatches
// agent.cancel to the guest and returns 202; the terminal `canceled` status
// arrives asynchronously via agent.done (observable on the task SSE / GET). It is
// idempotent: an already-terminal task is a 200 no-op (never re-dispatched). The
// partial working tree is left as-is for review via the git-review flow.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	tid, err := machine.ParseUUID(r.PathValue("tid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	task, err := s.Queries.GetAgentTask(r.Context(), tid)
	if err != nil || machine.UUIDString(task.MachineID) != machine.UUIDString(mc.ID) {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}

	// Already finished: nothing to cancel (idempotent no-op).
	if isTerminalTaskStatus(task.Status) {
		writeJSON(w, http.StatusOK, toTaskView(task))
		return
	}

	machineID := machine.UUIDString(mc.ID)
	taskID := machine.UUIDString(task.ID)
	if err := s.TaskChannel.CancelAgent(r.Context(), machineID, taskID); err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusBadGateway, "dispatch_failed")
		return
	}

	uid := uuidString(user.ID)
	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionAgentTaskCancel,
		Target:   taskID,
		Metadata: map[string]any{"project": task.Project},
	})
	writeJSON(w, http.StatusAccepted, taskIDResponse{TaskID: taskID})
}

// sendMessageRequest is the body of POST /api/machines/{id}/tasks/{tid}/messages.
type sendMessageRequest struct {
	Prompt string `json:"prompt"`
}

// handleSendMessage runs a follow-up turn on a finished task (AT4): it resumes
// the task's captured agent session (claude --resume) with a new prompt, reusing
// the same project + provider, and streams the new turn's events through the same
// task SSE. The task cycles back to `running` and on to a terminal state when the
// turn ends. 409 no_session if no session was ever captured; 409 task_running if
// a turn is already in flight.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	mc, err := s.resolveTerminalMachine(r.Context(), user, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	tid, err := machine.ParseUUID(r.PathValue("tid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}
	task, err := s.Queries.GetAgentTask(r.Context(), tid)
	if err != nil || machine.UUIDString(task.MachineID) != machine.UUIDString(mc.ID) {
		writeError(w, http.StatusNotFound, "no_task")
		return
	}

	// The prior turn must have finished (a turn can't start while one is in
	// flight), and a captured session is needed to resume.
	if task.Status == "running" || task.Status == "queued" {
		writeError(w, http.StatusConflict, "task_running")
		return
	}
	if task.AgentSessionID == "" {
		writeError(w, http.StatusConflict, "no_session")
		return
	}

	// The original provider must still be enabled, headless, and key-set.
	prov, err := s.Providers.Get(r.Context(), task.Provider)
	if err != nil || !prov.Enabled {
		writeError(w, http.StatusNotFound, "unknown_provider")
		return
	}
	if prov.LaunchCommand != "claude" {
		writeError(w, http.StatusBadRequest, "provider_not_headless")
		return
	}
	uid := uuidString(user.ID)
	if !s.providerKeySet(uid, task.Provider) {
		writeError(w, http.StatusConflict, "no_provider_key")
		return
	}

	machineID := machine.UUIDString(mc.ID)
	repoPath, code := s.resolveProject(r.Context(), machineID, task.Project)
	if code != "" {
		writeError(w, projectErrorStatus(code), code)
		return
	}
	if s.Injector != nil {
		if err := s.Injector.Inject(r.Context(), uid, machineID); err != nil {
			writeError(w, http.StatusBadGateway, "injection_failed")
			return
		}
	}

	taskID := machine.UUIDString(task.ID)
	if err := s.TaskChannel.RunAgent(r.Context(), machineID, taskID, repoPath, req.Prompt, task.Provider, task.AgentSessionID); err != nil {
		if errors.Is(err, guestctl.ErrNoChannel) {
			writeError(w, http.StatusConflict, "machine_not_running")
			return
		}
		writeError(w, http.StatusBadGateway, "dispatch_failed")
		return
	}
	// Reactivate the task's live stream for the new turn so a (re)connecting client
	// streams it instead of seeing the prior turn's terminal result and closing.
	if s.TaskEvents != nil {
		s.TaskEvents.Reopen(taskID)
	}
	// Dispatched: flip the task back to running and store this turn's prompt. The
	// captured session id is preserved (and refreshed by agent.done if rotated).
	_ = s.Queries.RestartAgentTask(r.Context(), store.RestartAgentTaskParams{ID: task.ID, Prompt: req.Prompt})

	s.Audit.Record(r.Context(), audit.Entry{
		UserID:   uid,
		Actor:    audit.UserActor(uid),
		Action:   audit.ActionAgentTaskMessage,
		Target:   task.Project,
		Metadata: map[string]any{"task_id": taskID, "provider": task.Provider},
	})
	writeJSON(w, http.StatusAccepted, taskIDResponse{TaskID: taskID})
}

func toTaskView(t store.AgentTask) taskView {
	v := taskView{
		ID:            machine.UUIDString(t.ID),
		Status:        t.Status,
		Provider:      t.Provider,
		Project:       t.Project,
		SessionID:     t.AgentSessionID,
		ResultSummary: t.ResultSummary,
		Error:         t.Error,
		CreatedAt:     tsString(t.CreatedAt),
		StartedAt:     tsString(t.StartedAt),
		EndedAt:       tsString(t.EndedAt),
	}
	if len(t.Usage) > 0 && string(t.Usage) != "{}" {
		v.Usage = json.RawMessage(t.Usage)
	}
	return v
}

func tsString(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
}
