package github

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// fakeTokenServer is an httptest stand-in for GitHub's token endpoint. It rotates
// the refresh token on every successful refresh and can be told to reject a
// refresh (revocation).
type fakeTokenServer struct {
	server       *httptest.Server
	refreshCount atomic.Int64
	mu           sync.Mutex
	rejectAll    bool  // every refresh returns bad_refresh_token
	expiresIn    int   // access token lifetime (s)
	nextRefresh  int64 // monotonic suffix for rotated refresh tokens
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	f := &fakeTokenServer{expiresIn: 28800}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		reject := f.rejectAll
		exp := f.expiresIn
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") != "refresh_token" {
			http.Error(w, "unexpected grant", http.StatusBadRequest)
			return
		}
		if reject || r.Form.Get("refresh_token") == "dead" {
			_, _ = w.Write([]byte(`{"error":"bad_refresh_token","error_description":"expired"}`))
			return
		}
		n := f.refreshCount.Add(1)
		// Slow the refresh slightly so the concurrency test can pile up callers.
		time.Sleep(20 * time.Millisecond)
		_, _ = fmt.Fprintf(w, `{"access_token":"gho_new_%d","refresh_token":"ghr_new_%d","token_type":"bearer","scope":"repo","expires_in":%d,"refresh_token_expires_in":15897600}`, n, n, exp)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// tsFixture wires a TokenSource against the fake server + a real (migrated) DB +
// a temp file secrets store, with one user linked.
type tsFixture struct {
	ts      *TokenSource
	secrets secrets.Store
	q       *store.Queries
	uid     string
	ref     string
	fake    *fakeTokenServer
}

func newTSFixture(t *testing.T) *tsFixture {
	t.Helper()
	pool, q := testutil.Postgres(t)
	fake := newFakeTokenServer(t)
	gh := NewClient(Config{ClientID: "id", ClientSecret: "secret", TokenURL: fake.server.URL + "/login/oauth/access_token"})

	sec, err := secrets.NewFileStore(t.TempDir() + "/secrets.json")
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}

	user, err := q.UpsertUser(context.Background(), store.UpsertUserParams{
		GithubUserID: 4242, Login: "octocat", Email: "octo@example.com", AvatarUrl: "",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	uid := uuidStr(user.ID)
	ref := secrets.UserGitHubPath(uid)
	_ = pool

	return &tsFixture{ts: NewTokenSource(gh, q, sec), secrets: sec, q: q, uid: uid, ref: ref, fake: fake}
}

// seed writes the token blob + link metadata.
func (f *tsFixture) seed(t *testing.T, access, refresh string, accessExp time.Time, revoked bool) {
	t.Helper()
	if err := f.secrets.Put(f.ref, map[string]string{
		fieldAccessToken:      access,
		fieldRefreshToken:     refresh,
		fieldTokenType:        "bearer",
		fieldScope:            "repo",
		fieldAccessExpiresAt:  accessExp.UTC().Format(time.RFC3339),
		fieldRefreshExpiresAt: time.Now().Add(180 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	var uid pgtype.UUID
	_ = uid.Scan(f.uid)
	meta, _ := json.Marshal(map[string]any{"revoked": revoked})
	if _, err := f.q.UpsertGitHubLink(context.Background(), store.UpsertGitHubLinkParams{
		UserID: uid, Metadata: meta, SecretRef: f.ref,
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
}

func (f *tsFixture) storedRefresh(t *testing.T) string {
	t.Helper()
	data, err := f.secrets.Get(f.ref)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	return data[fieldRefreshToken]
}

func TestTokenSource_FreshTokenNoRefresh(t *testing.T) {
	f := newTSFixture(t)
	f.seed(t, "gho_live", "ghr_live", time.Now().Add(2*time.Hour), false)

	cred, err := f.ts.Token(context.Background(), f.uid)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if cred.AccessToken != "gho_live" {
		t.Fatalf("expected live token returned, got %q", cred.AccessToken)
	}
	if n := f.fake.refreshCount.Load(); n != 0 {
		t.Fatalf("expected no refresh, got %d", n)
	}
}

func TestTokenSource_ExpiringRefreshesAndRotates(t *testing.T) {
	f := newTSFixture(t)
	// Within refreshSkew of expiry ⇒ must refresh.
	f.seed(t, "gho_old", "ghr_old", time.Now().Add(2*time.Minute), false)

	cred, err := f.ts.Token(context.Background(), f.uid)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !strings.HasPrefix(cred.AccessToken, "gho_new_") {
		t.Fatalf("expected refreshed token, got %q", cred.AccessToken)
	}
	if got := f.storedRefresh(t); !strings.HasPrefix(got, "ghr_new_") {
		t.Fatalf("rotated refresh token not persisted before release, stored=%q", got)
	}
	if n := f.fake.refreshCount.Load(); n != 1 {
		t.Fatalf("expected exactly one refresh, got %d", n)
	}
}

func TestTokenSource_RevokedMarksAndShortCircuits(t *testing.T) {
	f := newTSFixture(t)
	f.seed(t, "gho_old", "dead", time.Now().Add(1*time.Minute), false)

	_, err := f.ts.Token(context.Background(), f.uid)
	if err != ErrReconnectGitHub {
		t.Fatalf("expected ErrReconnectGitHub, got %v", err)
	}
	// Second call must short-circuit on the revoked flag (no server hit).
	before := f.fake.refreshCount.Load()
	_, err = f.ts.Token(context.Background(), f.uid)
	if err != ErrReconnectGitHub {
		t.Fatalf("expected ErrReconnectGitHub on second call, got %v", err)
	}
	if after := f.fake.refreshCount.Load(); after != before {
		t.Fatalf("revoked grant should not hit the server again")
	}
}

func TestTokenSource_ConcurrentSingleflight(t *testing.T) {
	f := newTSFixture(t)
	f.seed(t, "gho_old", "ghr_old", time.Now().Add(1*time.Minute), false)

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.ts.Token(context.Background(), f.uid)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := f.fake.refreshCount.Load(); got != 1 {
		t.Fatalf("singleflight: expected 1 refresh across %d callers, got %d", n, got)
	}
}

func TestTokenSource_NoTokenMaterialInLogs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newTSFixture(t)
	f.seed(t, "gho_old", "ghr_old", time.Now().Add(1*time.Minute), false)
	cred, err := f.ts.Token(context.Background(), f.uid)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	for _, secret := range []string{cred.AccessToken, "ghr_old", f.storedRefresh(t)} {
		if secret != "" && bytes.Contains(buf.Bytes(), []byte(secret)) {
			t.Fatalf("token material leaked into logs: %q", buf.String())
		}
	}
}

// uuidStr renders a pgtype.UUID canonically for test wiring.
func uuidStr(u pgtype.UUID) string {
	b := u.Bytes
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
