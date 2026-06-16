package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
		host string
		want string // "" ⇒ no match
	}{
		{"m-" + testMachineID + ".machines.example.com", testMachineID},
		{"m-" + testMachineID + ".machines.example.com:443", testMachineID},
		{"M-" + testMachineID + ".Machines.Example.Com", testMachineID}, // case-insensitive
		{"app.machines.example.com", ""},                                // not an m- label
		{"m-not-a-uuid.machines.example.com", ""},
		{"m-" + testMachineID + "-p8080.machines.example.com", ""}, // preview form reserved, not matched
		{"m-" + testMachineID + ".evil.com", ""},                   // wrong domain
		{"machines.example.com", ""},
	}
	for _, tc := range cases {
		got, ok := mw.parseHost(tc.host)
		if tc.want == "" {
			if ok {
				t.Errorf("parseHost(%q) matched %q, want no match", tc.host, got)
			}
			if mw.Matches(tc.host) {
				t.Errorf("Matches(%q) = true, want false", tc.host)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("parseHost(%q) = %q,%v want %q", tc.host, got, ok, tc.want)
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
