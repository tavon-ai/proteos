package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testMachineID = "11111111-1111-1111-1111-111111111111"

// stubGuests dials a fixed address as "the guest" (a stub code-server), honoring
// the web port the proxy requests.
type stubGuests struct{ addr string }

func (s stubGuests) DialGuest(_ context.Context, _ string, _ uint32) (net.Conn, error) {
	return net.Dial("tcp", s.addr)
}

type fakeSessions struct {
	owner string
	alive bool
}

func (f fakeSessions) SessionOwner(context.Context, string) (string, bool, error) {
	return f.owner, f.alive, nil
}

type fakeMachines struct {
	owner   string
	running bool
	exists  bool
}

func (f fakeMachines) MachineOwner(context.Context, string) (string, bool, bool, error) {
	return f.owner, f.running, f.exists, nil
}

// newTestMachineWeb builds a MachineWeb whose guest tunnel reaches backend, with
// the given session/machine resolver state. key is the HMAC signing key.
func newTestMachineWeb(t *testing.T, backendAddr string, sess fakeSessions, mach fakeMachines) (*MachineWeb, *Registry) {
	t.Helper()
	reg := NewRegistry()
	mw := NewMachineWeb(MachineWebConfig{
		Domain:         "localhost",
		SigningKey:     []byte("test-key-test-key-test-key-32byte"),
		CookieSecure:   false,
		FrameAncestors: []string{"http://localhost:5173"},
		Guests:         stubGuests{addr: backendAddr},
		Registry:       reg,
		Sessions:       sess,
		Machines:       mach,
	})
	if mw == nil {
		t.Fatal("NewMachineWeb returned nil")
	}
	return mw, reg
}

var testKey = []byte("test-key-test-key-test-key-32byte")

func TestParseHostAndMatches(t *testing.T) {
	mw := &MachineWeb{cfg: MachineWebConfig{Domain: "machines.example.com"}}
	cases := []struct {
		host     string
		want     string // "" ⇒ no match
		wantPort uint32
	}{
		{"m-" + testMachineID + ".machines.example.com", testMachineID, 0},
		{"m-" + testMachineID + ".machines.example.com:443", testMachineID, 0},
		{"M-" + testMachineID + ".Machines.Example.Com", testMachineID, 0}, // case-insensitive
		// PP1: the preview form now matches and yields the port.
		{"m-" + testMachineID + "-p8080.machines.example.com", testMachineID, 8080},
		{"m-" + testMachineID + "-p3000.machines.example.com:443", testMachineID, 3000},
		{"app.machines.example.com", "", 0}, // not an m- label
		{"m-not-a-uuid.machines.example.com", "", 0},
		{"m-" + testMachineID + "-p.machines.example.com", "", 0},      // -p with no digits
		{"m-" + testMachineID + "-p0.machines.example.com", "", 0},     // port 0 rejected
		{"m-" + testMachineID + "-p99999.machines.example.com", "", 0}, // out of uint16 range
		{"m-" + testMachineID + "-pxyz.machines.example.com", "", 0},   // junk port
		{"m-" + testMachineID + ".evil.com", "", 0},                    // wrong domain
		{"machines.example.com", "", 0},
	}
	for _, tc := range cases {
		got, port, ok := mw.parseHost(tc.host)
		if tc.want == "" {
			if ok {
				t.Errorf("parseHost(%q) matched %q, want no match", tc.host, got)
			}
			if mw.Matches(tc.host) {
				t.Errorf("Matches(%q) = true, want false", tc.host)
			}
			continue
		}
		if !ok || got != tc.want || port != tc.wantPort {
			t.Errorf("parseHost(%q) = %q,%d,%v want %q,%d", tc.host, got, port, ok, tc.want, tc.wantPort)
		}
	}
	// A nil handler matches nothing.
	var nilMW *MachineWeb
	if nilMW.Matches("m-" + testMachineID + ".machines.example.com") {
		t.Error("nil MachineWeb.Matches should be false")
	}
}

func TestMachineTokenAndCookieRoundTrip(t *testing.T) {
	now := time.Now()
	tok := machineToken{MachineID: testMachineID, UserID: "u1", SessionID: "s1", Exp: now.Add(30 * time.Second).Unix()}
	raw := signMachineToken(testKey, tok)
	got, err := verifyMachineToken(testKey, raw)
	if err != nil || got != tok {
		t.Fatalf("token roundtrip: got %+v err %v", got, err)
	}
	// Tamper: flip the last char of the payload.
	if _, err := verifyMachineToken(testKey, "x"+raw[1:]); err == nil {
		t.Error("tampered token verified")
	}
	// Wrong key.
	if _, err := verifyMachineToken([]byte("another-key-another-key-32bytes!"), raw); err == nil {
		t.Error("token verified under wrong key")
	}
	// Expired.
	expired := signMachineToken(testKey, machineToken{MachineID: testMachineID, SessionID: "s1", Exp: now.Add(-time.Second).Unix()})
	if _, err := verifyMachineToken(testKey, expired); err == nil {
		t.Error("expired token verified")
	}
	// Cookie roundtrip.
	ck := machineCookie{MachineID: testMachineID, SessionID: "s1", Exp: now.Add(time.Hour).Unix()}
	if got, err := verifyMachineCookie(testKey, signMachineCookie(testKey, ck)); err != nil || got != ck {
		t.Fatalf("cookie roundtrip: got %+v err %v", got, err)
	}
}

// stubBackend is an HTTP+WS server standing in for code-server.
func stubBackend(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r) // code-server has no control-plane API
			return
		}
		w.Header().Set("X-Editor", "stub")
		_, _ = w.Write([]byte("editor-ok"))
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
			_ = c.Write(r.Context(), typ, data)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// previewCookie mints a valid cookie scoped to a (machine, port) preview origin.
func previewCookie(t *testing.T, port uint32) *http.Cookie {
	t.Helper()
	val := signMachineCookie(testKey, machineCookie{
		MachineID: testMachineID, SessionID: "s1", Exp: time.Now().Add(time.Hour).Unix(), Port: port,
	})
	return &http.Cookie{Name: MachineCookieName, Value: val}
}

func validCookie(t *testing.T) *http.Cookie {
	t.Helper()
	val := signMachineCookie(testKey, machineCookie{
		MachineID: testMachineID, SessionID: "s1", Exp: time.Now().Add(time.Hour).Unix(),
	})
	return &http.Cookie{Name: MachineCookieName, Value: val}
}

func TestMachineWebAuthHandlerSetsCookie(t *testing.T) {
	mw, _ := newTestMachineWeb(t, stubBackend(t), fakeSessions{owner: "u1", alive: true}, fakeMachines{owner: "u1", running: true, exists: true})
	srv := httptest.NewServer(mw)
	defer srv.Close()

	tok := signMachineToken(testKey, machineToken{MachineID: testMachineID, UserID: "u1", SessionID: "s1", Exp: time.Now().Add(30 * time.Second).Unix()})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/__proteos/auth?token="+tok, nil)
	req.Host = "m-" + testMachineID + ".localhost"
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("auth status = %d, want 302", resp.StatusCode)
	}
	var set *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == MachineCookieName {
			set = c
		}
	}
	if set == nil {
		t.Fatal("auth did not set the machine cookie")
	}
	if set.SameSite != http.SameSiteNoneMode || !set.HttpOnly {
		t.Errorf("cookie attrs = SameSite %v HttpOnly %v; want None + HttpOnly", set.SameSite, set.HttpOnly)
	}
	// No folder in the token ⇒ redirect to the editor root.
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("no-folder redirect = %q, want /", loc)
	}

	// A bad token is 403.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/__proteos/auth?token=garbage", nil)
	req2.Host = "m-" + testMachineID + ".localhost"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("bad-token status = %d, want 403", resp2.StatusCode)
	}
}

// TestMachineWebAuthRedirectsToFolder verifies a token carrying a validated
// project folder redirects code-server to /?folder=<path> (Phase 9 decision #5).
func TestMachineWebAuthRedirectsToFolder(t *testing.T) {
	mw, _ := newTestMachineWeb(t, stubBackend(t), fakeSessions{owner: "u1", alive: true}, fakeMachines{owner: "u1", running: true, exists: true})
	srv := httptest.NewServer(mw)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	tok := signMachineToken(testKey, machineToken{
		MachineID: testMachineID, UserID: "u1", SessionID: "s1",
		Exp: time.Now().Add(30 * time.Second).Unix(), Folder: "/workspace/myrepo",
	})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/__proteos/auth?token="+tok, nil)
	req.Host = "m-" + testMachineID + ".localhost"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/?folder=%2Fworkspace%2Fmyrepo" {
		t.Errorf("folder redirect = %q, want /?folder=%%2Fworkspace%%2Fmyrepo", loc)
	}

	// A folder outside /workspace in the token is ignored (defence in depth): the
	// redirect falls back to the editor root rather than reflecting a bad path.
	bad := signMachineToken(testKey, machineToken{
		MachineID: testMachineID, UserID: "u1", SessionID: "s1",
		Exp: time.Now().Add(30 * time.Second).Unix(), Folder: "/etc",
	})
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/__proteos/auth?token="+bad, nil)
	req2.Host = "m-" + testMachineID + ".localhost"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if loc := resp2.Header.Get("Location"); loc != "/" {
		t.Errorf("bad-folder redirect = %q, want / (ignored)", loc)
	}
}

// TestMachineWebPreviewOrigin exercises the PP1 preview origin end-to-end at the
// gateway: the mint builds the -p<port> host and a port-bound token; auth on
// that host sets a port-scoped cookie and redirects to the app root; the proxy
// serves with that cookie. It also asserts cross-origin isolation — an editor
// cookie cannot be replayed on a preview host, and a token minted for one port
// cannot authenticate on another.
func TestMachineWebPreviewOrigin(t *testing.T) {
	const previewPort = 3000
	mw, _ := newTestMachineWeb(t, stubBackend(t), fakeSessions{"u1", true}, fakeMachines{"u1", true, true})
	// The test server's Domain is "localhost" (newTestMachineWeb), so hosts are
	// m-<uuid>[-p<port>].localhost.
	srv := httptest.NewServer(mw)
	defer srv.Close()
	previewHost := "m-" + testMachineID + "-p3000.localhost"
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Mint carries the -p3000 label and a port-bound token.
	rawURL := mw.MintWebSessionURL("http", testMachineID, "s1", "u1", "", previewPort)
	if !strings.Contains(rawURL, previewHost) {
		t.Fatalf("mint URL = %q, want host %q", rawURL, previewHost)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	authPathQuery := u.RequestURI()

	// Auth on the preview host sets a cookie and redirects to the app root.
	authReq, _ := http.NewRequest(http.MethodGet, srv.URL+authPathQuery, nil)
	authReq.Host = previewHost
	authResp, err := client.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	defer authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("preview auth = %d, want 302", authResp.StatusCode)
	}
	if loc := authResp.Header.Get("Location"); loc != "/" {
		t.Errorf("preview redirect = %q, want / (app root)", loc)
	}
	var previewCookie *http.Cookie
	for _, c := range authResp.Cookies() {
		if c.Name == MachineCookieName {
			previewCookie = c
		}
	}
	if previewCookie == nil {
		t.Fatal("preview auth did not set the machine cookie")
	}

	// Proxy with the port-scoped cookie reaches the backend.
	okReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	okReq.Host = previewHost
	okReq.AddCookie(previewCookie)
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatal(err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK || okResp.Header.Get("X-Editor") != "stub" {
		t.Fatalf("preview proxy = %d (X-Editor %q), want 200 from stub", okResp.StatusCode, okResp.Header.Get("X-Editor"))
	}

	// An editor cookie (port 0) replayed on the preview host is rejected.
	editorCookie := &http.Cookie{Name: MachineCookieName, Value: signMachineCookie(testKey, machineCookie{
		MachineID: testMachineID, SessionID: "s1", Exp: time.Now().Add(time.Hour).Unix(), // Port omitted ⇒ 0
	})}
	xReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	xReq.Host = previewHost
	xReq.AddCookie(editorCookie)
	xResp, err := http.DefaultClient.Do(xReq)
	if err != nil {
		t.Fatal(err)
	}
	defer xResp.Body.Close()
	if xResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("editor cookie on preview host = %d, want 401", xResp.StatusCode)
	}

	// The port-3000 token cannot authenticate on a port-9000 host.
	wrongReq, _ := http.NewRequest(http.MethodGet, srv.URL+authPathQuery, nil)
	wrongReq.Host = "m-" + testMachineID + "-p9000.localhost"
	wrongResp, err := client.Do(wrongReq)
	if err != nil {
		t.Fatal(err)
	}
	defer wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusForbidden {
		t.Fatalf("port-3000 token on port-9000 host = %d, want 403", wrongResp.StatusCode)
	}
}

func TestMachineWebProxyAuthzTable(t *testing.T) {
	backend := stubBackend(t)
	host := "m-" + testMachineID + ".localhost"

	cases := []struct {
		name     string
		sess     fakeSessions
		mach     fakeMachines
		cookie   bool
		badYet   bool // send a structurally-bad cookie
		wantCode int
	}{
		{"happy path", fakeSessions{"u1", true}, fakeMachines{"u1", true, true}, true, false, http.StatusOK},
		{"no cookie", fakeSessions{"u1", true}, fakeMachines{"u1", true, true}, false, false, http.StatusUnauthorized},
		{"bad cookie", fakeSessions{"u1", true}, fakeMachines{"u1", true, true}, false, true, http.StatusUnauthorized},
		{"session revoked", fakeSessions{"", false}, fakeMachines{"u1", true, true}, true, false, http.StatusForbidden},
		{"foreign machine", fakeSessions{"u1", true}, fakeMachines{"u2", true, true}, true, false, http.StatusForbidden},
		{"no machine", fakeSessions{"u1", true}, fakeMachines{"", false, false}, true, false, http.StatusForbidden},
		{"stopped machine", fakeSessions{"u1", true}, fakeMachines{"u1", false, true}, true, false, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw, _ := newTestMachineWeb(t, backend, tc.sess, tc.mach)
			srv := httptest.NewServer(mw)
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
			req.Host = host
			if tc.cookie {
				req.AddCookie(validCookie(t))
			}
			if tc.badYet {
				req.AddCookie(&http.Cookie{Name: MachineCookieName, Value: "not-a-valid-cookie"})
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}
}

func TestMachineWebProxiesToBackendAndApi404(t *testing.T) {
	mw, _ := newTestMachineWeb(t, stubBackend(t), fakeSessions{"u1", true}, fakeMachines{"u1", true, true})
	srv := httptest.NewServer(mw)
	defer srv.Close()
	host := "m-" + testMachineID + ".localhost"

	// Editor page proxies through with code-server's header + CSP frame-ancestors.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Host = host
	req.AddCookie(validCookie(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Editor") != "stub" {
		t.Error("response did not come from the stub backend")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors http://localhost:5173") {
		t.Errorf("CSP = %q, want frame-ancestors for the SPA origin", csp)
	}

	// /api/* on the subdomain reaches code-server (which 404s) — never the CP API.
	areq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	areq.Host = host
	areq.AddCookie(validCookie(t))
	aresp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	defer aresp.Body.Close()
	if aresp.StatusCode != http.StatusNotFound {
		t.Fatalf("/api/me on subdomain = %d, want 404", aresp.StatusCode)
	}
}

func TestMachineWebWebSocketOriginAndRevocation(t *testing.T) {
	mw, reg := newTestMachineWeb(t, stubBackend(t), fakeSessions{"u1", true}, fakeMachines{"u1", true, true})
	srv := httptest.NewServer(mw)
	defer srv.Close()
	host := "m-" + testMachineID + ".localhost"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// Wrong Origin is rejected (403) before the upgrade.
	hdrBad := http.Header{}
	hdrBad.Set("Host", host)
	hdrBad.Set("Origin", "https://evil.example")
	hdrBad.Set("Cookie", validCookie(t).String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdrBad, Host: host}); err == nil {
		t.Fatal("WS dial with foreign Origin should fail")
	}

	// Correct subdomain Origin succeeds and echoes; a revoke then closes it.
	hdr := http.Header{}
	hdr.Set("Origin", "http://"+host)
	hdr.Set("Cookie", validCookie(t).String())
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr, Host: host})
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil || string(data) != "ping" {
		t.Fatalf("ws echo: %q err %v", data, err)
	}

	// Revoking the parent session closes the live editor socket.
	reg.SessionRevoked("s1")
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected the editor socket to close after revocation")
	}
}

// TestMachineWebPreviewWebSocketOriginAndRevocation is the PP4 hardening check:
// the WS Origin enforcement and the revocation registry cover a preview origin
// exactly as they cover the editor. A foreign-origin upgrade to the preview host
// is rejected; an own-origin one succeeds and is closed the instant the parent
// session is revoked (owner logout).
func TestMachineWebPreviewWebSocketOriginAndRevocation(t *testing.T) {
	const previewPort = 3000
	mw, reg := newTestMachineWeb(t, stubBackend(t), fakeSessions{"u1", true}, fakeMachines{"u1", true, true})
	srv := httptest.NewServer(mw)
	defer srv.Close()
	previewHost := "m-" + testMachineID + "-p3000.localhost"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Foreign Origin (here, the editor origin) is rejected before the upgrade.
	hdrBad := http.Header{}
	hdrBad.Set("Origin", "http://m-"+testMachineID+".localhost")
	hdrBad.Set("Cookie", previewCookie(t, previewPort).String())
	if _, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdrBad, Host: previewHost}); err == nil {
		t.Fatal("preview WS dial with foreign Origin should fail")
	}

	// Own preview origin succeeds and echoes.
	hdr := http.Header{}
	hdr.Set("Origin", "http://"+previewHost)
	hdr.Set("Cookie", previewCookie(t, previewPort).String())
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr, Host: previewHost})
	if err != nil {
		t.Fatalf("preview WS dial: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("preview ws write: %v", err)
	}
	if _, data, err := c.Read(ctx); err != nil || string(data) != "ping" {
		t.Fatalf("preview ws echo: %q err %v", data, err)
	}

	// Owner logout (parent session revoke) closes the live preview socket.
	reg.SessionRevoked("s1")
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected the preview socket to close after revocation")
	}
}
