package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// TestMachineWebE2E exercises the whole Phase 8 path on a hypervisor-less dev
// stack: a real node-agent (DevDriver) runs a real guest agent whose web forward
// (port 1025, unsupervised) proxies to a stub "code-server"; the control plane
// mints a web-session token, the machine-web origin sets the subdomain cookie,
// and the reverse proxy carries an editor page + a WebSocket echo through the
// tunnel. It then proves a logout closes the live editor socket (revocation
// parity with terminals) and that /api/* on the subdomain never hits the CP API.
func TestMachineWebE2E(t *testing.T) {
	root := repoRoot(t)
	guestBin := buildBinary(t, filepath.Join(root, "guestagent"), "./cmd/guestagent")
	agentBin := buildBinary(t, filepath.Join(root, "nodeagent"), "./cmd/nodeagent")

	// Stub "code-server": serves an editor page, 404s /api/*, and echoes a WS.
	backendAddr := startEditorStub(t)

	const agentToken = "e2e-web-token"
	nodeURL := startNodeAgent(t, agentBin, guestBin, agentToken,
		"PROTEOS_DEV_GUEST_WEB_BACKEND="+backendAddr)
	nodes := nodeclient.New(nodeURL, agentToken)

	fx := setupCP(t, nodes, []string{testWSOrigin})

	ctx := context.Background()
	if _, err := nodes.Ensure(ctx, fx.machineID, agentapi.EnsureRequest{Vcpus: 1, MemMiB: 128, KernelRef: "k", RootfsRef: "r"}); err != nil {
		t.Fatalf("ensure on node-agent: %v", err)
	}
	t.Cleanup(func() { _ = nodes.Destroy(context.Background(), fx.machineID) })
	waitNodeRunning(t, nodes, fx.machineID)
	waitWebReachable(t, nodes, fx.machineID)

	machineHost := "m-" + fx.machineID + ".localhost"
	subdomainOrigin := "http://" + machineHost

	// 1. Mint a web-session URL from the main origin (requireAuth + CSRF).
	mintReq, _ := http.NewRequest(http.MethodPost, fx.url+"/api/machine/web-session", nil)
	mintReq.Header.Set("X-Requested-By", "proteos")
	mintReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	mintResp, err := http.DefaultClient.Do(mintReq)
	if err != nil {
		t.Fatalf("mint web-session: %v", err)
	}
	defer mintResp.Body.Close()
	if mintResp.StatusCode != http.StatusOK {
		t.Fatalf("mint status = %d, want 200", mintResp.StatusCode)
	}
	var mint struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(mintResp.Body).Decode(&mint); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if !strings.HasPrefix(mint.URL, subdomainOrigin+"/__proteos/auth?token=") {
		t.Fatalf("mint url = %q, want %s/__proteos/auth?token=…", mint.URL, subdomainOrigin)
	}
	token := mustTokenParam(t, mint.URL)

	// 2. Exchange the token at the machine origin for the subdomain cookie. The
	// request goes to the same httptest server but with the subdomain Host.
	authReq, _ := http.NewRequest(http.MethodGet, fx.url+"/__proteos/auth?token="+token, nil)
	authReq.Host = machineHost
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authResp, err := noFollow.Do(authReq)
	if err != nil {
		t.Fatalf("auth exchange: %v", err)
	}
	authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("auth status = %d, want 302", authResp.StatusCode)
	}
	var machineCookie *http.Cookie
	for _, c := range authResp.Cookies() {
		if c.Name == "proteos_machine" {
			machineCookie = c
		}
	}
	if machineCookie == nil {
		t.Fatal("auth did not set proteos_machine cookie")
	}

	// 3. Load the editor page through the proxy (over the 1025 tunnel → stub).
	pageReq, _ := http.NewRequest(http.MethodGet, fx.url+"/", nil)
	pageReq.Host = machineHost
	pageReq.AddCookie(machineCookie)
	pageResp, err := http.DefaultClient.Do(pageReq)
	if err != nil {
		t.Fatalf("editor page: %v", err)
	}
	body, _ := io.ReadAll(pageResp.Body)
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK || !strings.Contains(string(body), "editor-stub") {
		t.Fatalf("editor page = %d %q, want 200 editor-stub", pageResp.StatusCode, body)
	}

	// 4. /api/* on the subdomain reaches the stub (404) — never the CP API.
	apiReq, _ := http.NewRequest(http.MethodGet, fx.url+"/api/me", nil)
	apiReq.Host = machineHost
	apiReq.AddCookie(machineCookie)
	apiResp, err := http.DefaultClient.Do(apiReq)
	if err != nil {
		t.Fatalf("subdomain api: %v", err)
	}
	apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusNotFound {
		t.Fatalf("/api/me on subdomain = %d, want 404", apiResp.StatusCode)
	}

	// 5. Open the editor WebSocket through the proxy and echo a frame.
	wsURL := "ws" + strings.TrimPrefix(fx.url, "http") + "/ws"
	hdr := http.Header{}
	hdr.Set("Origin", subdomainOrigin)
	hdr.Set("Cookie", machineCookie.String())
	dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
	defer dcancel()
	wsConn, _, err := websocket.Dial(dctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr, Host: machineHost})
	if err != nil {
		t.Fatalf("editor ws dial: %v", err)
	}
	defer wsConn.CloseNow()
	if err := wsConn.Write(dctx, websocket.MessageText, []byte("hello-editor")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, echo, err := wsConn.Read(dctx)
	if err != nil || string(echo) != "hello-editor" {
		t.Fatalf("ws echo = %q err %v", echo, err)
	}

	// 6. Logout (revoke the parent session) closes the live editor socket.
	if err := fx.sessions.Revoke(ctx, fx.token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	if _, _, err := wsConn.Read(rctx); err == nil {
		t.Fatal("expected editor socket to close after logout")
	}

	// 7. After revocation, a fresh proxied request is rejected (403).
	postReq, _ := http.NewRequest(http.MethodGet, fx.url+"/", nil)
	postReq.Host = machineHost
	postReq.AddCookie(machineCookie)
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("post-revoke request: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusForbidden {
		t.Fatalf("post-revoke status = %d, want 403", postResp.StatusCode)
	}
}

// startEditorStub runs an HTTP+WS server standing in for code-server inside the
// VM. The guest agent's web forward proxies the 1025 tunnel to it.
func startEditorStub(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("editor-stub"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// waitWebReachable polls the guest web tunnel (port 1025) until it connects.
func waitWebReachable(t *testing.T, nodes *nodeclient.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := nodes.DialGuest(ctx, id, agentapi.GuestWebPort)
		cancel()
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("guest web forward never became reachable through the tunnel")
}

// mustTokenParam extracts the token query parameter from a web-session URL.
func mustTokenParam(t *testing.T, rawURL string) string {
	t.Helper()
	_, q, ok := strings.Cut(rawURL, "token=")
	if !ok {
		t.Fatalf("no token in url %q", rawURL)
	}
	return q
}
