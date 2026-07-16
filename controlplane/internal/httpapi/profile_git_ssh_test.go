package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/profile"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// recordingConfigurer records ReconfigureUser calls (Phase 4 git-identity re-apply).
type recordingConfigurer struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingConfigurer) ReconfigureUser(_ context.Context, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, userID)
}
func (r *recordingConfigurer) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestGitIdentityLifecycle(t *testing.T) {
	t.Parallel()
	fx := setupProfile(t)

	// Default identity is the GitHub one (fixture user: login "prof-user").
	var view map[string]any
	r0 := fx.do(t, http.MethodGet, "/api/profile/git", "", false)
	_ = json.NewDecoder(r0.Body).Decode(&view)
	r0.Body.Close()
	if view["source"] != "github" || view["name"] != "prof-user" || view["email"] != "p@example.com" {
		t.Fatalf("default identity = %v", view)
	}

	// Set a portable identity.
	r1 := fx.do(t, http.MethodPut, "/api/profile/git", `{"name":"Ada","email":"ada@example.com"}`, true)
	r1.Body.Close()
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("put: status %d", r1.StatusCode)
	}
	r2 := fx.do(t, http.MethodGet, "/api/profile/git", "", false)
	_ = json.NewDecoder(r2.Body).Decode(&view)
	r2.Body.Close()
	if view["source"] != "profile" || view["name"] != "Ada" || view["email"] != "ada@example.com" {
		t.Fatalf("profile identity = %v", view)
	}

	// Clear it → back to the GitHub default.
	r3 := fx.do(t, http.MethodDelete, "/api/profile/git", "", true)
	r3.Body.Close()
	if r3.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", r3.StatusCode)
	}
	r4 := fx.do(t, http.MethodGet, "/api/profile/git", "", false)
	_ = json.NewDecoder(r4.Body).Decode(&view)
	r4.Body.Close()
	if view["source"] != "github" {
		t.Fatalf("after clear, source = %v", view["source"])
	}
	// Deleting again is a 404 (nothing set).
	r5 := fx.do(t, http.MethodDelete, "/api/profile/git", "", true)
	r5.Body.Close()
	if r5.StatusCode != http.StatusNotFound {
		t.Fatalf("delete-absent: status %d, want 404", r5.StatusCode)
	}
}

func TestGitIdentityValidation(t *testing.T) {
	t.Parallel()
	fx := setupProfile(t)
	for _, body := range []string{`{"name":"","email":"a@b.com"}`, `{"name":"x","email":""}`, `{"name":"x","email":"no-at"}`} {
		resp := fx.do(t, http.MethodPut, "/api/profile/git", body, true)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status %d, want 422", body, resp.StatusCode)
		}
	}
	// CSRF required.
	resp := fx.do(t, http.MethodPut, "/api/profile/git", `{"name":"x","email":"a@b.com"}`, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403", resp.StatusCode)
	}
}

// TestGitIdentityReinjectsToRunningMachines proves a git-identity change calls the
// configurer to re-apply to running machines (parity with secret re-injection).
func TestGitIdentityReinjectsToRunningMachines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, q := testutil.Postgres(t)
	user, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 91, Login: "gitre", Email: "g@example.com"})
	sessions := session.NewManager(q, time.Hour)
	token, _ := sessions.Create(ctx, user.ID)
	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	cfg := &recordingConfigurer{}
	srv := &httpapi.Server{
		Sessions:      sessions,
		Queries:       q,
		Secrets:       sec,
		Audit:         rec,
		Profile:       profile.NewStore(q, sec, rec),
		GitConfigurer: cfg,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	do := func(method, body string) {
		req, _ := http.NewRequest(method, ts.URL+"/api/profile/git", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		req.Header.Set("X-Requested-By", "proteos")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	do(http.MethodPut, `{"name":"Ada","email":"ada@example.com"}`)
	if cfg.n() != 1 {
		t.Fatalf("set should reconfigure once, got %d", cfg.n())
	}
	do(http.MethodDelete, "")
	if cfg.n() != 2 {
		t.Fatalf("clear should reconfigure again, got %d", cfg.n())
	}
}

func TestSSHKeyLifecycle(t *testing.T) {
	t.Parallel()
	fx := setupProfile(t)

	// None to start.
	g0 := fx.do(t, http.MethodGet, "/api/profile/ssh", "", false)
	var view map[string]any
	_ = json.NewDecoder(g0.Body).Decode(&view)
	g0.Body.Close()
	if view["present"] != false {
		t.Fatalf("expected no key initially: %v", view)
	}

	// Generate.
	p := fx.do(t, http.MethodPost, "/api/profile/ssh", "", true)
	pbody, _ := io.ReadAll(p.Body)
	p.Body.Close()
	if p.StatusCode != http.StatusOK {
		t.Fatalf("generate: status %d", p.StatusCode)
	}
	if strings.Contains(string(pbody), "PRIVATE KEY") {
		t.Fatalf("generate response leaked the private key: %s", pbody)
	}
	var gen map[string]any
	_ = json.Unmarshal(pbody, &gen)
	pub, _ := gen["public_key"].(string)
	if !strings.HasPrefix(pub, "ssh-ed25519 ") || gen["fingerprint"] == "" {
		t.Fatalf("generate body = %v", gen)
	}

	// Private key stored in OpenBao (file content), public under sibling field.
	stored, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), profile.SSHKeyItemKey))
	if err != nil {
		t.Fatalf("ssh key not stored: %v", err)
	}
	if !strings.Contains(stored["value"], "OPENSSH PRIVATE KEY") {
		t.Fatalf("stored value is not an OpenSSH private key")
	}
	if stored["public"] != pub {
		t.Fatalf("stored public key mismatch")
	}
	// The SSH client config item exists too.
	cfg, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), profile.SSHConfigItemKey))
	if err != nil || !strings.Contains(cfg["value"], "StrictHostKeyChecking") {
		t.Fatalf("ssh config item missing/wrong: %v err=%v", cfg, err)
	}

	// The items list shows both as file-kind, with the key at .ssh/id_ed25519 0600.
	il := fx.do(t, http.MethodGet, "/api/profile/items", "", false)
	var items []map[string]any
	_ = json.NewDecoder(il.Body).Decode(&items)
	il.Body.Close()
	var sawKey bool
	for _, it := range items {
		if it["key"] == profile.SSHKeyItemKey {
			sawKey = true
			if it["kind"] != "file" || it["target"] != ".ssh/id_ed25519" || it["mode"] != "0600" {
				t.Fatalf("ssh key item view = %v", it)
			}
		}
	}
	if !sawKey {
		t.Fatalf("ssh key not in items list: %v", items)
	}

	// GET returns the public key (not the private).
	g1 := fx.do(t, http.MethodGet, "/api/profile/ssh", "", false)
	g1body, _ := io.ReadAll(g1.Body)
	g1.Body.Close()
	if strings.Contains(string(g1body), "PRIVATE KEY") {
		t.Fatalf("GET leaked private key: %s", g1body)
	}
	_ = json.Unmarshal(g1body, &view)
	if view["present"] != true || view["public_key"] != pub {
		t.Fatalf("GET ssh = %v", view)
	}

	// Delete removes the key + config.
	d := fx.do(t, http.MethodDelete, "/api/profile/ssh", "", true)
	d.Body.Close()
	if d.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", d.StatusCode)
	}
	if _, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), profile.SSHKeyItemKey)); err == nil {
		t.Fatal("ssh key should be gone after delete")
	}
	// Delete again → 404.
	d2 := fx.do(t, http.MethodDelete, "/api/profile/ssh", "", true)
	d2.Body.Close()
	if d2.StatusCode != http.StatusNotFound {
		t.Fatalf("delete-absent: status %d, want 404", d2.StatusCode)
	}
}
