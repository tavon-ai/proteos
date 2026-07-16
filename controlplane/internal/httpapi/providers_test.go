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

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/providers"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
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
	t.Parallel()
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
	// Assert the four Phase 6 seeds are present (by key, not by exact count: the
	// shared CI providers table may transiently hold rows from other tests).
	byKey := func(vs []map[string]any) map[string]map[string]any {
		m := map[string]map[string]any{}
		for _, v := range vs {
			m[v["key"].(string)] = v
		}
		return m
	}
	bk := byKey(views)
	for _, k := range []string{"claude", "gemini", "openai", "pi"} {
		if _, ok := bk[k]; !ok {
			t.Fatalf("seeded provider %q missing from %v", k, views)
		}
	}
	claude := bk["claude"]
	if claude["key_set"] != false || claude["enabled"] != true {
		t.Fatalf("claude should be enabled, key unset: %v", claude)
	}
	// The view carries field metadata so the UI can render a form from data.
	fields, ok := claude["secret_fields"].([]any)
	if !ok || len(fields) != 1 {
		t.Fatalf("claude secret_fields = %v", claude["secret_fields"])
	}
	f0 := fields[0].(map[string]any)
	if f0["name"] != "api_key" || f0["env"] != "ANTHROPIC_API_KEY" || f0["label"] == "" {
		t.Fatalf("claude field metadata wrong: %v", f0)
	}

	r2 := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"sk-live-123"}}`, true)
	r2.Body.Close()
	if r2.StatusCode != http.StatusNoContent {
		t.Fatalf("put key: status %d, want 204", r2.StatusCode)
	}

	r3 := fx.do(t, http.MethodGet, "/api/providers", "", false)
	defer r3.Body.Close()
	_ = json.NewDecoder(r3.Body).Decode(&views)
	if byKey(views)["claude"]["key_set"] != true {
		t.Fatalf("key_set should be true after put: %v", byKey(views)["claude"])
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
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/nope", `{"fields":{"api_key":"x"}}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown provider: status %d, want 404", resp.StatusCode)
	}
}

func TestPutEmptyKey422(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"   "}}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("empty key: status %d, want 422", resp.StatusCode)
	}
}

func TestPutRequiresCSRF(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"x"}}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403", resp.StatusCode)
	}
}

func TestDeleteProviderKey(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"sk-1"}}`, true).Body.Close()

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
	t.Parallel()
	fx := setupProviders(t)
	ctx := context.Background()

	fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"sk-secret-xyz"}}`, true).Body.Close()
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
	t.Parallel()
	fx := setupProviders(t)
	const key = "sk-must-not-leak-9999"

	put := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{"api_key":"`+key+`"}}`, true)
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

// TestPutUnknownField422 proves a field not declared by the provider is rejected.
func TestPutUnknownField422(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude",
		`{"fields":{"api_key":"sk-1","bogus":"x"}}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: status %d, want 422", resp.StatusCode)
	}
}

// TestPutMissingField422 proves an empty fields map (declared field absent) is
// rejected as a missing required field.
func TestPutMissingField422(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodPut, "/api/secrets/providers/claude", `{"fields":{}}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing field: status %d, want 422", resp.StatusCode)
	}
}

// TestSeededProvidersShape proves the Phase 6 seeds expose the expected field
// metadata and that openai carries a setup-style provider (no key in the view).
func TestSeededProvidersShape(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	resp := fx.do(t, http.MethodGet, "/api/providers", "", false)
	defer resp.Body.Close()
	var views []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]map[string]any{}
	for _, v := range views {
		byKey[v["key"].(string)] = v
	}
	for _, want := range []struct{ key, env string }{
		{"gemini", "GEMINI_API_KEY"},
		{"openai", "OPENAI_API_KEY"},
		{"pi", "ANTHROPIC_API_KEY"},
	} {
		v, ok := byKey[want.key]
		if !ok {
			t.Fatalf("provider %q not seeded; got %v", want.key, byKey)
		}
		fields := v["secret_fields"].([]any)
		if len(fields) != 1 {
			t.Fatalf("%s should declare one field, got %v", want.key, fields)
		}
		if env := fields[0].(map[string]any)["env"]; env != want.env {
			t.Fatalf("%s env = %v, want %v", want.key, env, want.env)
		}
	}
}

// TestPiKeyStoredUnderOwnPath proves pi's borrowed Anthropic key is stored under
// pi's own provider path, not read from or written to claude's (Phase 6 #2).
func TestPiKeyStoredUnderOwnPath(t *testing.T) {
	t.Parallel()
	fx := setupProviders(t)
	r := fx.do(t, http.MethodPut, "/api/secrets/providers/pi",
		`{"fields":{"anthropic_api_key":"sk-pi-borrowed"}}`, true)
	r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("put pi key: status %d, want 204", r.StatusCode)
	}
	pi, err := fx.sec.Get(secrets.UserProviderPath(fx.uid(), "pi"))
	if err != nil || pi["anthropic_api_key"] != "sk-pi-borrowed" {
		t.Fatalf("pi secret = %v, err = %v", pi, err)
	}
	if _, err := fx.sec.Get(secrets.UserProviderPath(fx.uid(), "claude")); err == nil {
		t.Fatal("claude path must remain untouched when setting pi's key")
	}
}
