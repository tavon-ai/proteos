package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	"github.com/tavon-ai/proteos/guestagent/internal/localsock"
	"github.com/tavon-ai/proteos/guestagent/internal/runas"
)

// stubResolver stands in for the control channel: it returns a fixed credential,
// the way the CP's git.credential handler would after resolving the user's token.
type stubResolver struct {
	username, password string
	reconnect          bool
	forbidden          bool
}

func (s stubResolver) Credential(_ context.Context, _, _ string) (guestwire.GitCredentialResponse, error) {
	if s.reconnect {
		return guestwire.GitCredentialResponse{}, &codeErr{guestwire.ErrCodeReconnectGitHub}
	}
	if s.forbidden {
		return guestwire.GitCredentialResponse{}, &codeErr{guestwire.ErrCodeForbiddenHost}
	}
	return guestwire.GitCredentialResponse{
		Username: s.username,
		Password: s.password,
		Expiry:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, nil
}

type codeErr struct{ code string }

func (e *codeErr) Error() string     { return e.code }
func (e *codeErr) ErrorCode() string { return e.code }

// TestGitCredentialHelper_CloneCommitPush is the Phase 7 executable acceptance
// for the in-guest path (task 7.5): the real `guestagent git-credential` helper,
// driven by real git, supplies a token on demand to a real smart-HTTP git server;
// clone → commit → push all succeed and NO token is ever written to disk.
func TestGitCredentialHelper_CloneCommitPush(t *testing.T) {
	gitBin, backend := gitTools(t)
	const token = "s3cr3t-token-value-xyz"

	// 1. Build the guestagent binary — it IS the credential helper git invokes.
	helperBin := buildGuestagent(t)

	// 2. A bare upstream repo served over smart HTTP, requiring the token as the
	//    HTTP basic-auth password (username x-access-token, GitHub-App style).
	root := t.TempDir()
	upstream := filepath.Join(root, "repo.git")
	runGit(t, gitBin, "", "init", "--bare", upstream)
	runGit(t, gitBin, upstream, "config", "http.receivepack", "true")

	srv := httptest.NewServer(authGate(token, &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}))
	t.Cleanup(srv.Close)
	repoURL := srv.URL + "/repo.git"

	// 3. The local helper socket + the agent-side resolver (the CP stand-in).
	sock := tmpSock(t)
	startLocalSock(t, sock, stubResolver{username: "x-access-token", password: token})

	// 4. A guest-like HOME with the gitconfig git.configure would have pushed —
	//    identity + the helper wiring, no secret.
	home := t.TempDir()
	writeGitConfig(t, home, helperBin)

	env := append(os.Environ(),
		"HOME="+home,
		"PROTEOS_AGENT_SOCK="+sock,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)

	// 5. Clone (the helper supplies the token; the URL embeds none).
	work := filepath.Join(t.TempDir(), "clone")
	runGitEnv(t, gitBin, "", env, "clone", repoURL, work)

	// 6. Commit using the configured identity, then push.
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello proteos\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitEnv(t, gitBin, work, env, "add", "README.md")
	runGitEnv(t, gitBin, work, env, "commit", "-m", "phase7: first commit")
	runGitEnv(t, gitBin, work, env, "push", "origin", "HEAD:refs/heads/main")

	// 7. The push landed upstream with the configured identity.
	out := runGit(t, gitBin, upstream, "log", "--format=%an <%ae> %s", "-1", "main")
	if !strings.Contains(out, "phase7: first commit") {
		t.Fatalf("push did not land: %q", out)
	}
	if !strings.Contains(out, "Ivan Pedrazas <ivan@example.com>") {
		t.Fatalf("commit identity not applied: %q", out)
	}

	// 8. The token must never have been written to disk anywhere in the guest.
	assertNoToken(t, home, token)
	assertNoToken(t, work, token)
}

// TestGitCredentialHelper_PublicHostAnonymousClone is the Gitea/Forgejo phase 1
// guest acceptance: against a host that serves reads anonymously (a public
// repo), git clone never invokes the credential helper and succeeds with no
// token; git push does trigger the helper, which the CP refuses for a
// non-auth host (forbidden_host) — surfaced as the anonymous-clone-only
// message, and the push fails.
func TestGitCredentialHelper_PublicHostAnonymousClone(t *testing.T) {
	gitBin, backend := gitTools(t)
	helperBin := buildGuestagent(t)

	root := t.TempDir()
	upstream := filepath.Join(root, "repo.git")
	runGit(t, gitBin, "", "init", "--bare", upstream)
	runGit(t, gitBin, upstream, "config", "http.receivepack", "true")

	srv := httptest.NewServer(publicReadGate(&cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}))
	t.Cleanup(srv.Close)
	repoURL := srv.URL + "/repo.git"

	// The resolver refuses every host — the CP's stance on public hosts. The
	// clone below still succeeding proves it never needed a credential.
	sock := tmpSock(t)
	startLocalSock(t, sock, stubResolver{forbidden: true})

	home := t.TempDir()
	writeGitConfig(t, home, helperBin)
	env := append(os.Environ(),
		"HOME="+home,
		"PROTEOS_AGENT_SOCK="+sock,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)

	work := filepath.Join(t.TempDir(), "clone")
	runGitEnv(t, gitBin, "", env, "clone", repoURL, work)

	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello public host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitEnv(t, gitBin, work, env, "add", "README.md")
	runGitEnv(t, gitBin, work, env, "commit", "-m", "phase1: public host")

	cmd := exec.Command(gitBin, "push", "origin", "HEAD:refs/heads/main")
	cmd.Dir = work
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("push to a public host succeeded; want a credential refusal:\n%s", out)
	}
	if !strings.Contains(string(out), "anonymous clone only") {
		t.Fatalf("push failure should carry the anonymous-clone-only message, got:\n%s", out)
	}
}

// publicReadGate requires auth only for receive-pack (pushes); reads are
// anonymous — the shape of a public repo on a Gitea/Forgejo host.
func publicReadGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "git-receive-pack") ||
			r.URL.Query().Get("service") == "git-receive-pack" {
			w.Header().Set("WWW-Authenticate", `Basic realm="proteos"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestGitCredentialHelper_RevokedFails proves a revoked grant makes the helper
// exit non-zero with reconnect_github on stderr (so git stops cleanly).
func TestGitCredentialHelper_RevokedFails(t *testing.T) {
	gitTools(t) // skip early if git is unavailable
	helperBin := buildGuestagent(t)
	sock := tmpSock(t)
	startLocalSock(t, sock, stubResolver{reconnect: true})

	cmd := exec.Command(helperBin, "git-credential", "get")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	cmd.Env = append(os.Environ(), "PROTEOS_AGENT_SOCK="+sock)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit on revoked grant, got success: %s", out)
	}
	if !strings.Contains(string(out), "reconnect") {
		t.Fatalf("expected a reconnect message on stderr, got: %s", out)
	}
	if strings.Contains(string(out), "password=") {
		t.Fatalf("helper printed a credential on a revoked grant: %s", out)
	}
}

// --- helpers ---------------------------------------------------------------

// gitTools locates git + git-http-backend, skipping the test when unavailable.
func gitTools(t *testing.T) (gitBin, backend string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed; skipping e2e")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	backend = filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not found; skipping e2e")
	}
	return gitBin, backend
}

func buildGuestagent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "guestagent")
	// No GOWORK override: the workspace supplies the go.sum entries for the
	// linux-only vsock deps, so the build works on the CI linux runner too.
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build guestagent: %v\n%s", err, out)
	}
	return bin
}

func startLocalSock(t *testing.T, path string, r localsock.Resolver) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := localsock.New(path, r, runas.Root())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx) }()
	// Wait for the socket to appear so git doesn't race the listener.
	for range 100 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-errc:
			t.Fatalf("local socket Serve failed: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("local socket never came up")
}

// tmpSock returns a short unix-socket path under /tmp. Unix socket paths have a
// ~104-char limit (macOS) / 108 (Linux), and t.TempDir() paths can exceed it.
func tmpSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ps")
	if err != nil {
		dir = t.TempDir()
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	return filepath.Join(dir, "a.sock")
}

// authGate wraps the git CGI handler with HTTP basic auth requiring the token as
// the password (username x-access-token), proving the credential is actually used.
func authGate(token string, next http.Handler) http.Handler {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="proteos"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeGitConfig(t *testing.T, home, helperBin string) {
	t.Helper()
	cfg := "[user]\n\tname = Ivan Pedrazas\n\temail = ivan@example.com\n" +
		"[credential]\n\thelper = " + helperBin + " git-credential\n\tuseHttpPath = false\n" +
		"[safe]\n\tdirectory = *\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, gitBin, dir string, args ...string) string {
	t.Helper()
	return runGitEnv(t, gitBin, dir, os.Environ(), args...)
}

func runGitEnv(t *testing.T, gitBin, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	if dir != "" {
		if strings.HasSuffix(dir, ".git") {
			cmd = exec.Command(gitBin, append([]string{"--git-dir", dir}, args...)...)
		} else {
			cmd.Dir = dir
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// assertNoToken walks dir and fails if the token appears in any file (e.g. a
// leaked .git-credentials or a token-embedded remote URL in .git/config).
func assertNoToken(t *testing.T, dir, token string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), token) {
			t.Errorf("token leaked to disk at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
