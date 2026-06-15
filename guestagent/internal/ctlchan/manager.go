package ctlchan

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/runas"
)

// credCacheTTL bounds how long a fetched credential is held in memory. A single
// `git fetch` invokes the helper several times; caching avoids a control-channel
// round trip per invocation while keeping the window short (decision #5). Nothing
// token-shaped is ever written to disk.
const credCacheTTL = 60 * time.Second

// cloneTimeout bounds a single git clone.
const cloneTimeout = 10 * time.Minute

// Manager owns the single live control channel for this guest and serves the
// credential lookups the local helper socket relays. The control plane dials
// HandleControl; while connected, Credential can resolve git tokens over it.
type Manager struct {
	homeDir string
	workDir string
	owner   runas.Identity // unprivileged user that clones run as / owns ~/.gitconfig

	mu     sync.Mutex
	active *conn

	cacheMu sync.Mutex
	cache   map[string]credEntry
}

type credEntry struct {
	resp    guestwire.GitCredentialResponse
	expires time.Time
}

// New builds a Manager. env is the guest shell environment (persist.ShellEnv):
// HOME=... and PROTEOS_WORKSPACE=... determine where ~/.gitconfig is written and
// where clones land. Sensible defaults apply when either is absent.
func New(env []string, owner runas.Identity) *Manager {
	home, work := "/root", "/workspace"
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "HOME":
			home = v
		case "PROTEOS_WORKSPACE":
			work = v
		}
	}
	return &Manager{homeDir: home, workDir: work, owner: owner, cache: map[string]credEntry{}}
}

// HandleControl upgrades GET /control and serves the control channel until the
// control plane disconnects. Exactly one channel is expected per machine; a new
// connection replaces any previous active one.
func (m *Manager) HandleControl(w http.ResponseWriter, r *http.Request) {
	// Not browser-facing (same trust boundary as /terminal): skip Origin checks.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		slog.Warn("control: ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20)

	conn := newConn(c, m.handle)
	m.setActive(conn)
	defer m.clearActive(conn)

	slog.Info("control channel up")
	err = conn.run(r.Context())
	slog.Info("control channel down", "err", err)
	c.Close(websocket.StatusNormalClosure, "")
}

func (m *Manager) setActive(c *conn) {
	m.mu.Lock()
	m.active = c
	m.mu.Unlock()
}

func (m *Manager) clearActive(c *conn) {
	m.mu.Lock()
	if m.active == c {
		m.active = nil
	}
	m.mu.Unlock()
	c.fail()
}

func (m *Manager) currentConn() *conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// handle dispatches inbound CP → guest requests.
func (m *Manager) handle(ctx context.Context, op string, payload json.RawMessage) (json.RawMessage, *guestwire.ControlErrorPayload) {
	switch op {
	case guestwire.OpGitConfigure:
		var p guestwire.GitConfigurePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "bad payload"}
		}
		if err := m.writeGitConfig(p); err != nil {
			slog.Error("control: git.configure failed", "err", err)
			return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "configure failed"}
		}
		slog.Info("control: gitconfig applied")
		return nil, nil

	case guestwire.OpGitClone:
		var p guestwire.GitClonePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "bad payload"}
		}
		if err := m.validateDest(p.Dest); err != nil {
			return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: err.Error()}
		}
		go m.runClone(p)
		return nil, nil // immediate ack; completion arrives as git.clone.done

	default:
		slog.Warn("control: unknown op", "op", op)
		return nil, &guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "unknown op: " + op}
	}
}

// writeGitConfig writes ~/.gitconfig idempotently (full overwrite). The file
// carries no secret — the token is fetched on demand by the helper.
func (m *Manager) writeGitConfig(p guestwire.GitConfigurePayload) error {
	helper := p.Helper
	if helper == "" {
		helper = guestwire.HelperBinPath
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[user]\n\tname = %s\n\temail = %s\n", p.Name, p.Email)
	fmt.Fprintf(&b, "[credential]\n\thelper = %s\n\tuseHttpPath = false\n", helper)
	fmt.Fprintf(&b, "[safe]\n\tdirectory = %s\n", filepath.Join(m.workDir, "*"))
	if err := os.MkdirAll(m.homeDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(m.homeDir, ".gitconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	// The agent writes this as root; hand it to the session user that reads it.
	if err := m.owner.Chown(path); err != nil {
		slog.Warn("control: chown .gitconfig failed", "path", path, "err", err)
	}
	return nil
}

// validateDest ensures the clone destination stays within the workspace tree.
func (m *Manager) validateDest(dest string) error {
	if dest == "" {
		return fmt.Errorf("empty dest")
	}
	clean := filepath.Clean(dest)
	root := filepath.Clean(m.workDir)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return fmt.Errorf("dest outside workspace")
	}
	return nil
}

// runClone clones a repo and reports the outcome over the channel. The token is
// supplied by the credential helper at fetch time; the URL holds no credential.
func (m *Manager) runClone(p guestwire.GitClonePayload) {
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()

	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		m.reportClone(p.OpID, false, "prepare workspace: "+err.Error())
		return
	}
	cmd := exec.CommandContext(ctx, "git", "clone", p.URL, p.Dest)
	cmd.Dir = m.workDir
	cmd.Env = append(os.Environ(), "HOME="+m.homeDir, "GIT_TERMINAL_PROMPT=0")
	// Clone as the unprivileged session user so the checked-out tree is owned by
	// it (the agent itself is root) — otherwise the user could not edit the repo
	// it is meant to work in. The credential helper git spawns then also runs as
	// that user and reaches the (user-owned) agent socket.
	if cred := m.owner.Credential(); cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("control: clone failed", "op_id", p.OpID, "err", err)
		m.reportClone(p.OpID, false, sanitizeCloneErr(out, err))
		return
	}
	slog.Info("control: clone done", "op_id", p.OpID, "dest", p.Dest)
	m.reportClone(p.OpID, true, "")
}

func (m *Manager) reportClone(opID string, ok bool, detail string) {
	c := m.currentConn()
	if c == nil {
		slog.Warn("control: cannot report clone — no channel", "op_id", opID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.notify(ctx, guestwire.OpGitCloneDone, guestwire.GitCloneDonePayload{OpID: opID, OK: ok, Detail: detail}); err != nil {
		slog.Warn("control: report clone failed", "op_id", opID, "err", err)
	}
}

// Credential resolves a git credential for host/protocol over the control
// channel, caching it in memory (≤credCacheTTL, never to disk). It is called by
// the local helper socket server. A revoked grant surfaces as a *ControlError
// with Code reconnect_github.
func (m *Manager) Credential(ctx context.Context, host, protocol string) (guestwire.GitCredentialResponse, error) {
	key := protocol + "://" + host
	if r, ok := m.cachedCred(key); ok {
		return r, nil
	}
	c := m.currentConn()
	if c == nil {
		return guestwire.GitCredentialResponse{}, &ControlError{Code: guestwire.ErrCodeUnavailable, Message: "no control channel"}
	}
	raw, err := c.request(ctx, guestwire.OpGitCredential, guestwire.GitCredentialRequest{Host: host, Protocol: protocol})
	if err != nil {
		return guestwire.GitCredentialResponse{}, err
	}
	var resp guestwire.GitCredentialResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return guestwire.GitCredentialResponse{}, err
	}
	m.storeCred(key, resp)
	return resp, nil
}

func (m *Manager) cachedCred(key string) (guestwire.GitCredentialResponse, bool) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	e, ok := m.cache[key]
	if !ok || time.Now().After(e.expires) {
		delete(m.cache, key)
		return guestwire.GitCredentialResponse{}, false
	}
	return e.resp, true
}

func (m *Manager) storeCred(key string, resp guestwire.GitCredentialResponse) {
	exp := time.Now().Add(credCacheTTL)
	if resp.Expiry != "" {
		if t, err := time.Parse(time.RFC3339, resp.Expiry); err == nil && t.Before(exp) {
			exp = t
		}
	}
	m.cacheMu.Lock()
	m.cache[key] = credEntry{resp: resp, expires: exp}
	m.cacheMu.Unlock()
}

// sanitizeCloneErr builds a short, non-sensitive failure detail. The clone URL
// embeds no token and the helper output never reaches git's stderr, but we still
// cap the size and keep only the final line.
func sanitizeCloneErr(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return err.Error()
	}
	lines := strings.Split(s, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 200 {
		last = last[:200]
	}
	return last
}
