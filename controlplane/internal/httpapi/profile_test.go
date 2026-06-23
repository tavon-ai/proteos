package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/profile"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// recordingInjector implements httpapi.Injector, recording the machine ids passed
// to InjectAsync so a test can assert re-injection targeted exactly the user's
// running machines. The fake is synchronous, so calls are recorded before the
// handler returns — no waiting on a goroutine.
type recordingInjector struct {
	mu    sync.Mutex
	async []string
}

func (r *recordingInjector) Inject(context.Context, string, string) error { return nil }
func (r *recordingInjector) InjectAsync(_ string, machineID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.async = append(r.async, machineID)
}
func (r *recordingInjector) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.async...)
}

type profileFixture struct {
	url    string
	token  string
	userID pgtype.UUID
	sec    *secrets.MemStore
	pool   *pgxpool.Pool
}

func setupProfile(t *testing.T) profileFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 71, Login: "prof-user", Email: "p@example.com",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Secrets:  sec,
		Audit:    rec,
		Profile:  profile.NewStore(q, sec, rec),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return profileFixture{url: ts.URL, token: token, userID: user.ID, sec: sec, pool: pool}
}

func (fx profileFixture) uid() string { return machine.UUIDString(fx.userID) }

func (fx profileFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, fx.url+path, r)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	if csrf {
		req.Header.Set("X-Requested-By", "proteos")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

const claudeItemPath = "/api/profile/items/" + profile.ClaudeOAuthKey

// TestProfileSetListDelete exercises the full Phase 1 lifecycle: set stores the
// value in OpenBao + a metadata row; list returns metadata only; delete removes
// both. The value never appears in Postgres or any response.
func TestProfileSetListDelete(t *testing.T) {
	fx := setupProfile(t)
	const tok = "claude-oauth-token-abc123"

	// Empty list to start.
	r0 := fx.do(t, http.MethodGet, "/api/profile/items", "", false)
	var initial []map[string]any
	_ = json.NewDecoder(r0.Body).Decode(&initial)
	r0.Body.Close()
	if len(initial) != 0 {
		t.Fatalf("expected no items initially, got %v", initial)
	}

	// Set the Claude OAuth token.
	r1 := fx.do(t, http.MethodPut, claudeItemPath, `{"value":"`+tok+`"}`, true)
	r1.Body.Close()
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("put: status %d, want 204", r1.StatusCode)
	}

	// The value lives in OpenBao under the profile path.
	stored, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), profile.ClaudeOAuthKey))
	if err != nil {
		t.Fatalf("value not stored in OpenBao: %v", err)
	}
	if stored["value"] != tok {
		t.Fatalf("stored value = %v", stored)
	}

	// A metadata row exists; it must NOT carry the value in any column.
	var key, kind, target string
	if err := fx.pool.QueryRow(context.Background(),
		"SELECT key, kind, target FROM profile_items WHERE user_id=$1 AND key=$2",
		fx.userID, profile.ClaudeOAuthKey).Scan(&key, &kind, &target); err != nil {
		t.Fatalf("metadata row missing: %v", err)
	}
	if kind != string(profile.KindEnv) || target != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("metadata wrong: kind=%q target=%q", kind, target)
	}

	// List returns the item's metadata, never the value.
	r2 := fx.do(t, http.MethodGet, "/api/profile/items", "", false)
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if bytes.Contains(body, []byte(tok)) {
		t.Fatalf("list response leaked the token: %s", body)
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["key"] != profile.ClaudeOAuthKey {
		t.Fatalf("list = %v", items)
	}
	if items[0]["connected"] != true || items[0]["kind"] != "env" {
		t.Fatalf("item view = %v", items[0])
	}
	if items[0]["expires_at"] == nil || items[0]["expires_at"] == "" {
		t.Fatalf("expected expires_at for the 1y token, got %v", items[0]["expires_at"])
	}

	// Delete removes both the value and the row.
	r3 := fx.do(t, http.MethodDelete, claudeItemPath, "", true)
	r3.Body.Close()
	if r3.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", r3.StatusCode)
	}
	if _, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), profile.ClaudeOAuthKey)); err == nil {
		t.Fatal("value should be gone after delete")
	}
	var rows int
	_ = fx.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM profile_items WHERE user_id=$1 AND key=$2",
		fx.userID, profile.ClaudeOAuthKey).Scan(&rows)
	if rows != 0 {
		t.Fatalf("metadata row should be gone after delete, found %d", rows)
	}
}

// TestProfileReinjectsRunningMachines proves Phase 2's lifecycle gap is closed:
// connecting (and disconnecting) a token re-injects to the user's currently-
// running machines — and only those — so the change takes effect without a
// recreate. A stopped machine and another user's running machine are never targeted.
func TestProfileReinjectsRunningMachines(t *testing.T) {
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 81, Login: "reinj"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 82, Login: "other"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(owner pgtype.UUID, state string) string {
		m, err := q.CreateMachine(ctx, store.CreateMachineParams{
			UserID: owner, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte("{}"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "UPDATE machines SET state=$2 WHERE id=$1", m.ID, state); err != nil {
			t.Fatal(err)
		}
		return machine.UUIDString(m.ID)
	}
	runningID := mk(user.ID, "running")
	_ = mk(user.ID, "stopped")  // same user, not running → must be skipped
	_ = mk(other.ID, "running") // other user, running → must never be targeted

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.NewMemStore()
	rec := audit.NewRecorder(q)
	inj := &recordingInjector{}
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Secrets:  sec,
		Audit:    rec,
		Profile:  profile.NewStore(q, sec, rec),
		Injector: inj,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	do := func(method string) {
		req, _ := http.NewRequest(method, ts.URL+claudeItemPath, strings.NewReader(`{"value":"tok-abc"}`))
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
		req.Header.Set("X-Requested-By", "proteos")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: status %d, want 204", method, resp.StatusCode)
		}
	}

	do(http.MethodPut)
	if got := inj.calls(); len(got) != 1 || got[0] != runningID {
		t.Fatalf("after connect: re-injected %v, want exactly [%s]", got, runningID)
	}

	do(http.MethodDelete)
	if got := inj.calls(); len(got) != 2 || got[1] != runningID {
		t.Fatalf("after disconnect: re-injected %v, want second call to %s", got, runningID)
	}
}

// TestProfileNeedsReconnectWhenExpired proves the list surfaces a known-expired
// token (from its metadata expiry) as needs_reconnect rather than reporting it as
// healthily connected. A freshly-set token (1y TTL) is not flagged.
func TestProfileNeedsReconnectWhenExpired(t *testing.T) {
	fx := setupProfile(t)

	fx.do(t, http.MethodPut, claudeItemPath, `{"value":"fresh-token"}`, true).Body.Close()

	needsReconnect := func() bool {
		r := fx.do(t, http.MethodGet, "/api/profile/items", "", false)
		defer r.Body.Close()
		var items []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("want one item, got %v", items)
		}
		return items[0]["needs_reconnect"] == true
	}

	if needsReconnect() {
		t.Fatal("a freshly-set 1y token must not be needs_reconnect")
	}

	// Force the metadata expiry into the past (a token past its TTL).
	if _, err := fx.pool.Exec(context.Background(),
		"UPDATE profile_items SET expires_at = now() - interval '1 day' WHERE user_id=$1 AND key=$2",
		fx.userID, profile.ClaudeOAuthKey); err != nil {
		t.Fatal(err)
	}
	if !needsReconnect() {
		t.Fatal("an expired token must surface needs_reconnect")
	}
}

func TestProfilePutUnknownItem404(t *testing.T) {
	fx := setupProfile(t)
	resp := fx.do(t, http.MethodPut, "/api/profile/items/nope", `{"value":"x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown item: status %d, want 404", resp.StatusCode)
	}
}

// TestProfileFileItemLifecycle exercises a generic file-kind item (Phase 3): an
// unregistered key with kind:"file" + path + mode stores the content in OpenBao
// and path/mode/kind in Postgres; the list surfaces them (never the content);
// delete removes both. The content never appears in Postgres or any response.
func TestProfileFileItemLifecycle(t *testing.T) {
	fx := setupProfile(t)
	const content = "[user]\n\temail = ada@example.com\n"
	const path = "/api/profile/items/gitconfig"

	r1 := fx.do(t, http.MethodPut, path, `{"kind":"file","path":".gitconfig","mode":420,"value":`+jsonString(content)+`}`, true)
	r1.Body.Close()
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("put file item: status %d, want 204", r1.StatusCode)
	}

	// Content in OpenBao under the profile path.
	stored, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), "gitconfig"))
	if err != nil || stored["value"] != content {
		t.Fatalf("file content not stored exactly: %v err=%v", stored, err)
	}

	// Metadata row carries kind=file, target=path, mode — never the content.
	var kind, target string
	var mode *int32
	if err := fx.pool.QueryRow(context.Background(),
		"SELECT kind, target, mode FROM profile_items WHERE user_id=$1 AND key=$2",
		fx.userID, "gitconfig").Scan(&kind, &target, &mode); err != nil {
		t.Fatalf("metadata row: %v", err)
	}
	if kind != "file" || target != ".gitconfig" || mode == nil || *mode != 0o644 {
		t.Fatalf("metadata wrong: kind=%q target=%q mode=%v", kind, target, mode)
	}

	// List exposes path+mode, not the content.
	r2 := fx.do(t, http.MethodGet, "/api/profile/items", "", false)
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if bytes.Contains(body, []byte("ada@example.com")) {
		t.Fatalf("list leaked file content: %s", body)
	}
	var items []map[string]any
	_ = json.Unmarshal(body, &items)
	if len(items) != 1 || items[0]["kind"] != "file" || items[0]["target"] != ".gitconfig" || items[0]["mode"] != "0644" {
		t.Fatalf("file item view = %v", items)
	}

	// Delete removes it.
	r3 := fx.do(t, http.MethodDelete, path, "", true)
	r3.Body.Close()
	if r3.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", r3.StatusCode)
	}
	if _, err := fx.sec.Get(secrets.UserProfilePath(fx.uid(), "gitconfig")); err == nil {
		t.Fatal("content should be gone after delete")
	}
}

// TestProfileFileItemRejectsEscape proves a $HOME-escaping path is rejected (422)
// and an unregistered key without kind:"file" is a 404 (not a silent generic item).
func TestProfileFileItemRejectsEscape(t *testing.T) {
	fx := setupProfile(t)
	esc := fx.do(t, http.MethodPut, "/api/profile/items/evil",
		`{"kind":"file","path":"../../etc/cron.d/x","value":"x"}`, true)
	esc.Body.Close()
	if esc.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("escaping path: status %d, want 422", esc.StatusCode)
	}
	noKind := fx.do(t, http.MethodPut, "/api/profile/items/whatever", `{"value":"x"}`, true)
	noKind.Body.Close()
	if noKind.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered non-file: status %d, want 404", noKind.StatusCode)
	}
}

// TestProfileDeleteUnknown404 proves deleting an item the user does not have is a
// 404 (the row-count gate), not a false 204.
func TestProfileDeleteUnknown404(t *testing.T) {
	fx := setupProfile(t)
	resp := fx.do(t, http.MethodDelete, claudeItemPath, "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete absent item: status %d, want 404", resp.StatusCode)
	}
}

// jsonString renders s as a JSON string literal (so embedded newlines/quotes are
// escaped) for building request bodies in tests.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestProfilePutEmptyValue422(t *testing.T) {
	fx := setupProfile(t)
	resp := fx.do(t, http.MethodPut, claudeItemPath, `{"value":"   "}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty value: status %d, want 422", resp.StatusCode)
	}
}

func TestProfilePutRequiresCSRF(t *testing.T) {
	fx := setupProfile(t)
	resp := fx.do(t, http.MethodPut, claudeItemPath, `{"value":"x"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403", resp.StatusCode)
	}
}

// TestProfileAuditNoLeak asserts put/delete write audit rows whose target is the
// path, never the value.
func TestProfileAuditNoLeak(t *testing.T) {
	fx := setupProfile(t)
	const tok = "tok-must-not-leak-7777"

	fx.do(t, http.MethodPut, claudeItemPath, `{"value":"`+tok+`"}`, true).Body.Close()
	fx.do(t, http.MethodDelete, claudeItemPath, "", true).Body.Close()

	rows, err := fx.pool.Query(context.Background(), "SELECT action, target FROM audit_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sawPut, sawDelete bool
	for rows.Next() {
		var action, target string
		if err := rows.Scan(&action, &target); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(target, tok) {
			t.Fatalf("audit target leaked the value: %q", target)
		}
		switch action {
		case audit.ActionSecretPut:
			sawPut = true
		case audit.ActionSecretDelete:
			sawDelete = true
		}
	}
	if !sawPut || !sawDelete {
		t.Fatalf("missing audit rows: put=%v delete=%v", sawPut, sawDelete)
	}
}
