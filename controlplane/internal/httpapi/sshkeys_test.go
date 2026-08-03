package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/sshkeys"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

type sshKeysFixture struct {
	url       string
	token     string
	userID    pgtype.UUID
	machineID string
	injector  *recordingInjector
}

func setupSSHKeys(t *testing.T) sshKeysFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{
		GithubUserID: 81, Login: "sshkeys-user", Email: "s@example.com",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local-sshkeys", AgentUrl: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	m, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}
	// reinjectRunningMachines only targets running machines — force this one so
	// add/revoke have something to fan out to.
	if _, err := pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", m.ID); err != nil {
		t.Fatalf("set running: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	inj := &recordingInjector{}

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		SSHKeys:  sshkeys.NewStore(q),
		Injector: inj,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return sshKeysFixture{
		url: ts.URL, token: token, userID: user.ID,
		machineID: machine.UUIDString(m.ID), injector: inj,
	}
}

func (fx sshKeysFixture) uid() string { return machine.UUIDString(fx.userID) }

func (fx sshKeysFixture) do(t *testing.T, method, path, body string, csrf bool) *http.Response {
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

// genPublicKey mints a fresh ed25519 keypair and returns its authorized_keys
// line — a stand-in for what a real user would paste from their laptop.
func genPublicKey(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n") + " " + comment
}

// TestSSHLoginKeyLifecycle exercises add → list → revoke end to end through the
// HTTP API, and proves each mutation re-injects to the user's running machines
// (parity with the outbound git-SSH key and generic profile items).
func TestSSHLoginKeyLifecycle(t *testing.T) {
	t.Parallel()
	fx := setupSSHKeys(t)

	// Empty to start.
	l0 := fx.do(t, http.MethodGet, "/api/ssh-keys", "", false)
	var list []map[string]any
	_ = json.NewDecoder(l0.Body).Decode(&list)
	l0.Body.Close()
	if len(list) != 0 {
		t.Fatalf("expected no keys initially: %v", list)
	}

	pub := genPublicKey(t, "laptop")
	body, _ := json.Marshal(map[string]string{"label": "My laptop", "public_key": pub})
	a := fx.do(t, http.MethodPost, "/api/ssh-keys", string(body), true)
	abody, _ := io.ReadAll(a.Body)
	a.Body.Close()
	if a.StatusCode != http.StatusOK {
		t.Fatalf("add: status %d body %s", a.StatusCode, abody)
	}
	var added map[string]any
	_ = json.Unmarshal(abody, &added)
	if added["label"] != "My laptop" || added["public_key"] != pub || added["fingerprint"] == "" || added["id"] == "" {
		t.Fatalf("add response = %v", added)
	}
	if calls := fx.injector.calls(); len(calls) != 1 || calls[0] != fx.machineID {
		t.Fatalf("add should reinject the user's one running machine once, got %v", calls)
	}

	l1 := fx.do(t, http.MethodGet, "/api/ssh-keys", "", false)
	_ = json.NewDecoder(l1.Body).Decode(&list)
	l1.Body.Close()
	if len(list) != 1 || list[0]["id"] != added["id"] {
		t.Fatalf("list after add = %v", list)
	}

	id, _ := added["id"].(string)
	d := fx.do(t, http.MethodDelete, "/api/ssh-keys/"+id, "", true)
	d.Body.Close()
	if d.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status %d", d.StatusCode)
	}
	if len(fx.injector.calls()) != 2 {
		t.Fatalf("revoke should reinject again, got %v", fx.injector.calls())
	}

	l2 := fx.do(t, http.MethodGet, "/api/ssh-keys", "", false)
	_ = json.NewDecoder(l2.Body).Decode(&list)
	l2.Body.Close()
	if len(list) != 0 {
		t.Fatalf("list after revoke = %v", list)
	}

	// Revoking again is a 404 (already gone).
	d2 := fx.do(t, http.MethodDelete, "/api/ssh-keys/"+id, "", true)
	d2.Body.Close()
	if d2.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke-absent: status %d, want 404", d2.StatusCode)
	}
}

// TestSSHLoginKeyValidation proves the API rejects what the store rejects: an
// unparsable key (422) and a duplicate active fingerprint (409); and enforces
// CSRF on the mutating routes.
func TestSSHLoginKeyValidation(t *testing.T) {
	t.Parallel()
	fx := setupSSHKeys(t)

	bad, _ := json.Marshal(map[string]string{"label": "x", "public_key": "not a key"})
	r := fx.do(t, http.MethodPost, "/api/ssh-keys", string(bad), true)
	r.Body.Close()
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid key: status %d, want 422", r.StatusCode)
	}

	pub := genPublicKey(t, "dup")
	body, _ := json.Marshal(map[string]string{"label": "one", "public_key": pub})
	r1 := fx.do(t, http.MethodPost, "/api/ssh-keys", string(body), true)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first add: status %d", r1.StatusCode)
	}
	r2 := fx.do(t, http.MethodPost, "/api/ssh-keys", string(body), true)
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate add: status %d, want 409", r2.StatusCode)
	}

	// CSRF required for POST.
	body2, _ := json.Marshal(map[string]string{"label": "two", "public_key": genPublicKey(t, "two")})
	r3 := fx.do(t, http.MethodPost, "/api/ssh-keys", string(body2), false)
	r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: status %d, want 403", r3.StatusCode)
	}
}

// TestSSHLoginKeyOwnershipIsolated proves one user cannot revoke another
// user's key (404, not a leak of its existence), and that lists never cross
// users.
func TestSSHLoginKeyOwnershipIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, q := testutil.Postgres(t)

	userA, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 82, Login: "owner-a"})
	userB, _ := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 83, Login: "owner-b"})
	sessions := session.NewManager(q, time.Hour)
	tokA, _ := sessions.Create(ctx, userA.ID)
	tokB, _ := sessions.Create(ctx, userB.ID)

	srv := &httpapi.Server{
		Sessions: sessions,
		Queries:  q,
		Audit:    audit.NewRecorder(q),
		SSHKeys:  sshkeys.NewStore(q),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	doAs := func(tok, method, path, body string, csrf bool) *http.Response {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, r)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tok})
		if csrf {
			req.Header.Set("X-Requested-By", "proteos")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	body, _ := json.Marshal(map[string]string{"label": "a's key", "public_key": genPublicKey(t, "a")})
	a := doAs(tokA, http.MethodPost, "/api/ssh-keys", string(body), true)
	var added map[string]any
	_ = json.NewDecoder(a.Body).Decode(&added)
	a.Body.Close()
	id, _ := added["id"].(string)

	// B cannot see A's key.
	lb := doAs(tokB, http.MethodGet, "/api/ssh-keys", "", false)
	var listB []map[string]any
	_ = json.NewDecoder(lb.Body).Decode(&listB)
	lb.Body.Close()
	if len(listB) != 0 {
		t.Fatalf("user B sees user A's keys: %v", listB)
	}

	// B cannot revoke A's key.
	db := doAs(tokB, http.MethodDelete, "/api/ssh-keys/"+id, "", true)
	db.Body.Close()
	if db.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user revoke: status %d, want 404", db.StatusCode)
	}

	// It is still there for A.
	la := doAs(tokA, http.MethodGet, "/api/ssh-keys", "", false)
	var listA []map[string]any
	_ = json.NewDecoder(la.Body).Decode(&listA)
	la.Body.Close()
	if len(listA) != 1 {
		t.Fatalf("user A's key was removed by a foreign revoke attempt: %v", listA)
	}
}
