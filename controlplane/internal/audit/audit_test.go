package audit

import (
	"context"
	"testing"
)

func TestUserActor(t *testing.T) {
	if got := UserActor("abc-123"); got != "user:abc-123" {
		t.Errorf("UserActor = %q, want user:abc-123", got)
	}
}

func TestRecordNilSafe(t *testing.T) {
	// A nil Recorder, and a Recorder with a nil store, must both be no-ops:
	// auditing is best-effort and must never panic on a partially-wired server.
	var r *Recorder
	r.Record(context.Background(), Entry{Action: ActionSecretPut})

	r2 := NewRecorder(nil)
	r2.Record(context.Background(), Entry{
		Actor:    ActorSystemInjector,
		Action:   ActionSecretRead,
		Target:   "providers/claude",
		Metadata: map[string]any{"k": "v"},
	})
}

func TestActionConstants(t *testing.T) {
	// Guard against accidental edits to the canonical action strings written to
	// audit_log.action (callers and dashboards depend on these exact values).
	cases := map[string]string{
		ActionSecretPut:     "secret.put",
		ActionSecretDelete:  "secret.delete",
		ActionSecretRead:    "secret.read",
		ActionAgentLaunch:   "agent.launch",
		ActionGitRepos:      "git.repos",
		ActionGitClone:      "git.clone",
		ActionGitCredential: "git.credential",
		ActionGitPRMerge:    "git.pr.merge",
		ActionGitPRComment:  "git.pr.comment",
		ActionGitHostTokenSet:    "git.host.token.set",
		ActionGitHostTokenDelete: "git.host.token.delete",
		ActorSystemInjector: "system:injector",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("constant = %q, want %q", got, want)
		}
	}
}
