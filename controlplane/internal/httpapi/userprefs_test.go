package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// TestUserPrefs_ClaudeAttribution exercises PATCH /api/user/preferences for the
// claude_attribution toggle: it defaults to enabled, persists, and a real change
// re-pushes the configure ops to the user's running machines — while a no-op
// PATCH or an unrelated preference change does not.
func TestUserPrefs_ClaudeAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 72, Login: "prefs-user", Email: "prefs@example.com",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rc := &recordingConfigurer{}
	srv := &httpapi.Server{
		Sessions:      sessions,
		Queries:       q,
		Audit:         audit.NewRecorder(q),
		GitConfigurer: rc,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	patch := func(body string) map[string]any {
		t.Helper()
		fx := profileFixture{url: ts.URL, token: token, userID: user.ID}
		resp := fx.do(t, http.MethodPatch, "/api/user/preferences", body, true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %s: status %d", body, resp.StatusCode)
		}
		var prefs map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&prefs); err != nil {
			t.Fatalf("decode prefs: %v", err)
		}
		return prefs
	}

	// Defaults: attribution on, inbound SSH off (opt-in), no reconfigure from an
	// empty PATCH.
	prefs := patch(`{}`)
	if prefs["claude_attribution"] != true || prefs["download_as_is"] != false {
		t.Fatalf("default prefs = %v", prefs)
	}
	if prefs["ssh_enabled"] != false {
		t.Fatalf("inbound SSH defaults to on, want opt-in: %v", prefs)
	}
	if rc.n() != 0 {
		t.Fatalf("empty PATCH reconfigured machines %d times", rc.n())
	}

	// Turning attribution off persists and re-pushes once.
	prefs = patch(`{"claude_attribution":false}`)
	if prefs["claude_attribution"] != false {
		t.Fatalf("after disable, prefs = %v", prefs)
	}
	if rc.n() != 1 {
		t.Fatalf("disable should reconfigure once, got %d", rc.n())
	}
	if prefs = patch(`{}`); prefs["claude_attribution"] != false {
		t.Fatalf("preference did not persist: %v", prefs)
	}

	// A no-op PATCH (same value) does not re-push.
	patch(`{"claude_attribution":false}`)
	if rc.n() != 1 {
		t.Fatalf("no-op PATCH reconfigured machines, total %d", rc.n())
	}

	// An unrelated preference change does not re-push either.
	prefs = patch(`{"download_as_is":true}`)
	if prefs["download_as_is"] != true || prefs["claude_attribution"] != false {
		t.Fatalf("after download change, prefs = %v", prefs)
	}
	if rc.n() != 1 {
		t.Fatalf("download change reconfigured machines, total %d", rc.n())
	}

	// The inbound-SSH switch round-trips and persists. It re-injects rather than
	// reconfiguring git, so the git configurer must stay untouched.
	prefs = patch(`{"ssh_enabled":true}`)
	if prefs["ssh_enabled"] != true {
		t.Fatalf("after enabling SSH, prefs = %v", prefs)
	}
	if prefs = patch(`{}`); prefs["ssh_enabled"] != true {
		t.Fatalf("SSH switch did not persist: %v", prefs)
	}
	if prefs = patch(`{"ssh_enabled":false}`); prefs["ssh_enabled"] != false {
		t.Fatalf("after disabling SSH, prefs = %v", prefs)
	}
	if rc.n() != 1 {
		t.Fatalf("SSH switch reconfigured git, total %d", rc.n())
	}
}
