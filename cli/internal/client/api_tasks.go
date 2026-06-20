package client

import (
	"context"
	"encoding/json"
)

// Task mirrors the control-plane taskView. Result fields are populated only once
// the run is terminal.
type Task struct {
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

// IsTerminal reports whether the task has reached a final state.
func (t Task) IsTerminal() bool {
	switch t.Status {
	case "done", "failed", "canceled":
		return true
	default:
		return false
	}
}

// Failed reports whether the task ended unsuccessfully (failed or canceled),
// which the CLI maps to ExitTaskFail.
func (t Task) Failed() bool {
	return t.Status == "failed" || t.Status == "canceled"
}

// CreateTaskRequest is the POST /api/machines/{id}/tasks body.
type CreateTaskRequest struct {
	Prompt   string `json:"prompt"`
	Provider string `json:"provider"`
	Project  string `json:"project"`
}

type taskIDResponse struct {
	TaskID string `json:"task_id"`
}

type tasksResponse struct {
	Tasks []Task `json:"tasks"`
}

// CreateTask dispatches a headless agent run and returns its task id.
func (c *Client) CreateTask(ctx context.Context, machineID string, req CreateTaskRequest) (string, error) {
	var r taskIDResponse
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/tasks", req, &r)
	return r.TaskID, err
}

// ListTasks returns a machine's tasks, newest first.
func (c *Client) ListTasks(ctx context.Context, machineID string) ([]Task, error) {
	var r tasksResponse
	err := c.Do(ctx, "GET", "/api/machines/"+machineID+"/tasks", nil, &r)
	return r.Tasks, err
}

// GetTask returns one task's status + result.
func (c *Client) GetTask(ctx context.Context, machineID, taskID string) (Task, error) {
	var t Task
	err := c.Do(ctx, "GET", "/api/machines/"+machineID+"/tasks/"+taskID, nil, &t)
	return t, err
}

// CancelTask requests cancellation of a running task (idempotent server-side).
func (c *Client) CancelTask(ctx context.Context, machineID, taskID string) (string, error) {
	var r taskIDResponse
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/tasks/"+taskID+"/cancel", struct{}{}, &r)
	return r.TaskID, err
}

// SendMessage runs a follow-up turn on a finished task (resume).
func (c *Client) SendMessage(ctx context.Context, machineID, taskID, prompt string) (string, error) {
	var r taskIDResponse
	body := struct {
		Prompt string `json:"prompt"`
	}{Prompt: prompt}
	err := c.Do(ctx, "POST", "/api/machines/"+machineID+"/tasks/"+taskID+"/messages", body, &r)
	return r.TaskID, err
}
