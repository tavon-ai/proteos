// Package audit appends rows to the audit_log table (Phase 5 decision #6, the
// early slice of Phase 10's audit surface). The Recorder is intentionally
// best-effort: an audit failure logs but never fails the operation it records,
// so a transient DB hiccup cannot block a user from saving a key. Secret values
// are NEVER passed here — targets are paths, metadata is non-sensitive.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// Actions are the canonical action strings written to audit_log.action.
const (
	ActionSecretPut    = "secret.put"    // a user set/replaced a provider key
	ActionSecretDelete = "secret.delete" // a user removed a provider key
	ActionSecretRead   = "secret.read"   // the injector read a secret for injection
	ActionAgentLaunch  = "agent.launch"  // a user launched an agent session

	// AT1/AT3/AT4 headless task lane. Target is the project name (run/message) or task id (cancel).
	ActionAgentTaskRun     = "agent.task.run"     // a user dispatched a headless agent task
	ActionAgentTaskCancel  = "agent.task.cancel"  // a user canceled a running task
	ActionAgentTaskMessage = "agent.task.message" // a user sent a follow-up turn (resume)

	// Phase 7 git actions. Targets are repo full-names / hosts — never tokens.
	ActionGitRepos      = "git.repos"      // a user listed their accessible repos
	ActionGitClone      = "git.clone"      // a user cloned a repo into a machine
	ActionGitCredential = "git.credential" // the credential handler minted a git token

	// GR2–GR5 git mutations. Target is the project name — never a token.
	ActionGitBranch   = "git.branch" // a user created/checked out a branch in a project
	ActionGitCommit   = "git.commit" // a user committed changes in a project
	ActionGitPush     = "git.push"   // a user pushed a branch to origin (also the SSE event type)
	ActionGitPRCreate = "git.pr"     // a user opened a pull request (also the SSE event type)
)

// Actor prefixes identify who performed the action.
const (
	ActorSystemInjector = "system:injector"
)

// UserActor returns the actor string for a user-initiated action.
func UserActor(userID string) string { return "user:" + userID }

// Recorder writes audit rows.
type Recorder struct {
	q *store.Queries
}

// NewRecorder returns a Recorder backed by q.
func NewRecorder(q *store.Queries) *Recorder { return &Recorder{q: q} }

// Entry is one audit row. UserID is the canonical UUID string, or "" for a
// system actor (stored as NULL). Target is a path/identifier, never a secret
// value. Metadata is optional non-sensitive context.
type Entry struct {
	UserID   string
	Actor    string
	Action   string
	Target   string
	Metadata map[string]any
}

// Record appends entry. Errors are logged, not returned: auditing must not break
// the audited operation.
func (r *Recorder) Record(ctx context.Context, e Entry) {
	if r == nil || r.q == nil {
		return
	}
	meta := []byte("{}")
	if len(e.Metadata) > 0 {
		if b, err := json.Marshal(e.Metadata); err == nil {
			meta = b
		}
	}
	var uid pgtype.UUID
	if e.UserID != "" {
		if err := uid.Scan(e.UserID); err != nil {
			slog.Warn("audit: bad user id", "err", err)
		}
	}
	if _, err := r.q.InsertAuditLog(ctx, store.InsertAuditLogParams{
		UserID:   uid,
		Actor:    e.Actor,
		Action:   e.Action,
		Target:   e.Target,
		Metadata: meta,
	}); err != nil {
		slog.Error("audit: insert failed", "action", e.Action, "err", err)
	}
}
