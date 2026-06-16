package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/term"
)

// newCwdTestServer starts a server whose workspace root is a temp dir, so cwd
// validation can be exercised without a real /workspace. It returns the server
// and the workspace root.
func newCwdTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	mgr := term.NewManager(term.Config{Shell: "/bin/bash", ScrollbackKiB: 256})
	t.Cleanup(mgr.Shutdown)
	srv := New(mgr, nil, nil, nil)
	srv.workspaceRoot = root
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, root
}

func cwdURL(ts *httptest.Server, session, cwd string) string {
	q := url.Values{}
	q.Set(guestwire.QueryParamSession, session)
	q.Set(guestwire.QueryParamCwd, cwd)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/terminal?" + q.Encode()
}

// TestSessionStartsInCwd verifies two sessions with distinct opaque names and
// distinct cwds each land in the right directory (pwd over the PTY).
func TestSessionStartsInCwd(t *testing.T) {
	ts, root := newCwdTestServer(t)
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	for _, d := range []string{alpha, beta} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	check := func(session, dir string) {
		c := dial(t, cwdURL(ts, session, dir))
		defer c.Close(websocket.StatusNormalClosure, "")
		readHello(t, c)
		sendInput(t, c, "pwd\n")
		// macOS symlinks /var → /private/var; match on the trailing path segment.
		readBinaryUntil(t, c, filepath.Base(dir), 5*time.Second)
	}
	check("win-alpha", alpha)
	check("win-beta", beta)
}

// TestBadCwdRejected verifies the guest rejects a cwd outside the workspace or
// one that does not exist with a 400 (no upgrade).
func TestBadCwdRejected(t *testing.T) {
	ts, root := newCwdTestServer(t)
	cases := map[string]string{
		"outside workspace": "/etc",
		"nonexistent":       filepath.Join(root, "does-not-exist"),
		"escape":            filepath.Join(root, "..", "etc"),
	}
	for name, cwd := range cases {
		t.Run(name, func(t *testing.T) {
			q := url.Values{}
			q.Set(guestwire.QueryParamSession, "w")
			q.Set(guestwire.QueryParamCwd, cwd)
			resp, err := http.Get(ts.URL + "/terminal?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestNoCwdUnchanged verifies the existing no-cwd path still works (regression
// guard for Phase 3/5).
func TestNoCwdUnchanged(t *testing.T) {
	ts := newTestServer(t)
	c := dial(t, wsURL(ts, "main"))
	defer c.Close(websocket.StatusNormalClosure, "")
	readHello(t, c)
	sendInput(t, c, "echo still-works\n")
	readBinaryUntil(t, c, "still-works", 5*time.Second)
}
