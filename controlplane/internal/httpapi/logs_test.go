package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/applog"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

type logsFixture struct {
	url   string
	token string
	logs  *applog.Store
}

func setupLogs(t *testing.T) logsFixture {
	t.Helper()
	ctx := context.Background()
	_, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 91, Login: "logs-user", Email: "logs@example.com",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	logs := applog.NewStore(100)
	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Logs:     logs,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return logsFixture{url: ts.URL, token: token, logs: logs}
}

func (fx logsFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
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

// TestListLogs_FiltersBySourceAndRequiresAuth exercises GET /api/logs: it
// requires authentication, defaults to returning every source, and ?source=
// narrows to just "api" or "ui".
func TestListLogs_FiltersBySourceAndRequiresAuth(t *testing.T) {
	fx := setupLogs(t)
	fx.logs.Add(applog.Entry{Time: time.Now(), Level: "INFO", Source: "api", Message: "http request"})
	fx.logs.Add(applog.Entry{Time: time.Now(), Level: "ERROR", Source: "ui", Message: "window crash"})

	// Unauthenticated → 401.
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/api/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated GET: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", resp.StatusCode)
	}

	// Default: both sources.
	resp = fx.do(t, http.MethodGet, "/api/logs", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/logs status = %d", resp.StatusCode)
	}
	var body struct {
		Entries []applog.Entry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(body.Entries))
	}

	// source=ui narrows to just the UI entry.
	resp = fx.do(t, http.MethodGet, "/api/logs?source=ui", "", false)
	defer resp.Body.Close()
	body.Entries = nil
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Source != "ui" {
		t.Fatalf("source=ui entries = %+v", body.Entries)
	}

	// An invalid source is rejected.
	resp = fx.do(t, http.MethodGet, "/api/logs?source=firecracker", "", false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad source status = %d, want 400", resp.StatusCode)
	}
}

// TestExportLogs_DownloadsPlainTextAttachment exercises GET /api/logs/export:
// it requires auth, needs no CSRF header (a GET), and returns a text/plain
// attachment containing every captured entry.
func TestExportLogs_DownloadsPlainTextAttachment(t *testing.T) {
	fx := setupLogs(t)
	fx.logs.Add(applog.Entry{Time: time.Now(), Level: "INFO", Source: "api", Message: "control plane listening"})

	resp := fx.do(t, http.MethodGet, "/api/logs/export", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition = %q", cd)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(out), "control plane listening") {
		t.Fatalf("export body missing entry: %q", out)
	}
}

// TestReportUILog_RequiresCSRFAndCapturesEntry exercises POST /api/logs/ui: it
// is CSRF-guarded like every cookie-authed mutation, validates the body, and
// the captured entry shows up via GET /api/logs tagged source "ui" with the
// reporting user's login attached.
func TestReportUILog_RequiresCSRFAndCapturesEntry(t *testing.T) {
	fx := setupLogs(t)

	// Missing CSRF header → 403.
	resp := fx.do(t, http.MethodPost, "/api/logs/ui", `{"level":"error","message":"boom"}`, false)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-CSRF status = %d, want 403", resp.StatusCode)
	}

	// Empty message → 400.
	resp = fx.do(t, http.MethodPost, "/api/logs/ui", `{"level":"error","message":""}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty message status = %d, want 400", resp.StatusCode)
	}

	// Valid report → 204, then visible via GET /api/logs.
	resp = fx.do(t, http.MethodPost, "/api/logs/ui",
		`{"level":"error","message":"window crash","fields":{"component":"desktop"}}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("valid report status = %d, want 204", resp.StatusCode)
	}

	resp = fx.do(t, http.MethodGet, "/api/logs?source=ui", "", false)
	defer resp.Body.Close()
	var body struct {
		Entries []applog.Entry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("expected 1 ui entry, got %d", len(body.Entries))
	}
	e := body.Entries[0]
	if e.Level != "ERROR" || e.Message != "window crash" || e.Fields["component"] != "desktop" {
		t.Fatalf("captured entry = %+v", e)
	}
	if e.Fields["user"] != "logs-user" {
		t.Fatalf("captured entry missing reporting user: %+v", e)
	}
}
