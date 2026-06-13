package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/providers"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

type provFixture struct {
	url    string
	token  string
	userID pgtype.UUID
	sec    *secrets.MemStore
	pool   *pgxpool.Pool
}

func setupProviders(t *testing.T) provFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 7, Login: "sec-user", Email: "s@example.com",
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

	srv := &httpapi.Server{
		Sessions:  sessions,
		Queries:   q,
		Providers: providers.NewRegistry(q),
		Secrets:   sec,
		Audit:     audit.NewRecorder(q),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return provFixture{url: ts.URL, token: token, userID: user.ID, sec: sec, pool: pool}
}

func (fx provFixture) uid() string { return machine.UUIDString(fx.userID) }

func (fx provFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
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

func TestProvidersListAndKeySet(t *testing.T) {
	fx := setupProviders(t)

	resp := fx.do(t, http.MethodGet, "/api/providers", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d", resp.StatusCode)
	}
	var views []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0]["key"] != "claude" {
		t.Fatalf("expected seeded claude provider, got %v", views)
	}
	if views[0]["key_set"] != false || views[0]["enabled"] != true {
		t.Fatalf("claude should be enabled, key unset: %v", views[0])
	}

	r2 := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"sk-live-123"}`, true)
	r2.Body.Close()
	if r2.StatusCode != http.StatusNoContent {
		t.Fatalf("put key: status %d, want 204", r2.StatusCode)
	}

	r3 := fx.do(t, http.MethodGet, "/api/providers", "", false)
	defer r3.Body.Close()
	_ = json.NewDecoder(r3.Body).Decode(&views)
	if views[0]["key_set"] != true {
		t.Fatalf("key_set should be true after put: %v", views[0])
	}

	stored, err := fx.sec.Get(secrets.UserProviderPath(fx.uid(), "claude"))
	if err != nil {
		t.Fatalf("secret not stored: %v", err)
	}
	if stored["api_key"] != "sk-live-123" {
		t.Fatalf("stored secret = %v", stored)
	}
}

func TestPutUnknownProvider404(t *testing.T) {
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/nope", `{"api_key":"x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider: status %d, want 404", resp.StatusCode)
	}
}

func TestPutEmptyKey422(t *testing.T) {
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"   "}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty key: status %d, want 422", resp.StatusCode)
	}
}

func TestPutRequiresCSRF(t *testing.T) {
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"x"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403", resp.StatusCode)
	}
}

func TestDeleteProviderKey(t *testing.T) {
	fx := setupProviders(t)
	fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"sk-1"}`, true).Body.Close()

	resp := fx.do(t, http.MethodDelete, "/api/secrets/providers/claude", "", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d, want 204", resp.StatusCode)
	}
	if _, err := fx.sec.Get(secrets.UserProviderPath(fx.uid(), "claude")); err == nil {
		t.Fatal("secret should be gone after delete")
	}
}

// TestAuditRowsOnPutDelete asserts an audit row is written for both put and
// delete, with the path (never the value) as target.
func TestAuditRowsOnPutDelete(t *testing.T) {
	fx := setupProviders(t)
	ctx := context.Background()

	fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"sk-secret-xyz"}`, true).Body.Close()
	fx.do(t, http.MethodDelete, "/api/secrets/providers/claude", "", true).Body.Close()

	rows, err := fx.pool.Query(ctx, "SELECT action, target FROM audit_log ORDER BY id")
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
		if strings.Contains(target, "sk-secret-xyz") {
			t.Fatalf("audit target leaked the key: %q", target)
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

// TestKeyNeverInResponse scans providers/secrets response bodies for the key.
func TestKeyNeverInResponse(t *testing.T) {
	fx := setupProviders(t)
	const key = "sk-must-not-leak-9999"

	put := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"api_key":"`+key+`"}`, true)
	b, _ := io.ReadAll(put.Body)
	put.Body.Close()

	list := fx.do(t, http.MethodGet, "/api/providers", "", false)
	b2, _ := io.ReadAll(list.Body)
	list.Body.Close()

	for _, body := range [][]byte{b, b2} {
		if bytes.Contains(body, []byte(key)) {
			t.Fatalf("key leaked in response: %s", body)
		}
	}
}
