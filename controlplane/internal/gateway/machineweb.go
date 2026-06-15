package gateway

// machineweb is the Phase 8 per-machine editor origin. It is the control plane's
// first HTTP reverse-proxy path (until now the gateway only relayed WebSocket
// terminals) and the place the web-origin-isolation decision becomes structural:
// a machine's editor is served from m-<uuid>.<domain>, an origin that serves ONLY
// /__proteos/auth and the code-server proxy — never /api, never the SPA. The main
// host never serves these paths and the subdomain never serves the API, so a
// proxy bug on one origin cannot reach routes on the other (decision #1).
//
// Auth (decision #2/#3): the main origin mints a ≤60s HMAC token; the SPA points
// the editor frame at /__proteos/auth?token=…; this handler validates it and sets
// a subdomain-scoped, signed cookie bound to the PARENT session id (the main
// session cookie is host-only and never arrives — fact #1). Every later request
// re-checks cookie signature → parent session alive → user still owns the machine
// before proxying, so logout/revocation invalidates the editor immediately.

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// MachineCookieName is the subdomain-scoped editor cookie. Exported for the
// origin-isolation regression test (8.0), which asserts the MAIN session cookie
// never lands on a machine host while this one is the only cookie that does.
const MachineCookieName = "proteos_machine"

const (
	// webTokenTTL bounds the one-shot mint→auth window (decision #2: ≤60s).
	webTokenTTL = 60 * time.Second
	// machineCookieTTL is the editor cookie's backstop lifetime; the real gate is
	// the per-request parent-session-alive check, so this is generous.
	machineCookieTTL = 12 * time.Hour
)

// machineLabelRe matches the m-<uuid> host label (decision #1). The full UUID
// keeps the label valid (38 chars) and not trivially guessable; authz (not
// obscurity) is load-bearing. The m-<uuid>-p<port> preview form is reserved for
// Phase 9+ and deliberately does NOT match here.
var machineLabelRe = regexp.MustCompile(`^m-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// SessionResolver reports the user that owns a still-live parent session, by id.
// *session.Manager.AliveByID satisfies an adapter for this (kept string-typed so
// the gateway stays decoupled from store/pgtype).
type SessionResolver interface {
	// SessionOwner returns the owning user id for a live session, or ok=false if
	// the session is revoked/expired/unknown.
	SessionOwner(ctx context.Context, sessionID string) (userID string, ok bool, err error)
}

// MachineResolver reports a machine's owner and running state.
type MachineResolver interface {
	// MachineOwner returns the machine's owner user id and whether it is running.
	// ok=false ⇒ no such machine.
	MachineOwner(ctx context.Context, machineID string) (userID string, running bool, ok bool, err error)
}

// MachineWebConfig wires the machine-web handler.
type MachineWebConfig struct {
	Domain         string      // parent domain (m-<uuid>.<Domain>); empty ⇒ disabled
	SigningKey     []byte      // HMAC key (reuses StateSigningKey)
	CookieSecure   bool        // Secure attribute on the editor cookie
	FrameAncestors []string    // origins allowed to frame the editor (the SPA origins)
	Guests         GuestDialer // dials the guest tunnel (web port)
	Registry       *Registry   // revocation registry (shared with terminals)
	Sessions       SessionResolver
	Machines       MachineResolver
}

// MachineWeb serves the per-machine editor origin. A nil *MachineWeb (domain
// unset) disables machine-web routing entirely.
type MachineWeb struct {
	cfg   MachineWebConfig
	proxy *httputil.ReverseProxy
}

// NewMachineWeb builds the handler, or nil when Domain is empty (feature off).
func NewMachineWeb(cfg MachineWebConfig) *MachineWeb {
	if cfg.Domain == "" {
		return nil
	}
	mw := &MachineWeb{cfg: cfg}

	transport := &http.Transport{
		// Each connection is a fresh guest tunnel to the machine encoded in the
		// dial address (the Director sets req.URL.Host = machineID). Pooling keys
		// on that host, so distinct machines never share a tunnel.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			return cfg.Guests.DialGuest(ctx, host, agentapi.GuestWebPort)
		},
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	mw.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Director: func(r *http.Request) {
			machineID := r.Header.Get(internalMachineHeader)
			r.Header.Del(internalMachineHeader)
			// Dial/cache key is the machine id; the Host header code-server sees
			// stays the original subdomain so its Origin==Host WebSocket check
			// passes (the historically fragile part of proxying code-server).
			r.URL.Scheme = "http"
			r.URL.Host = machineID
			r.Header.Set("X-Forwarded-Host", r.Host)
			r.Header.Set("X-Forwarded-Proto", forwardedProto(r))
		},
		ModifyResponse: func(resp *http.Response) error {
			// Only the configured SPA origins may frame the editor (decision #3).
			if fa := strings.Join(cfg.FrameAncestors, " "); fa != "" {
				resp.Header.Set("Content-Security-Policy", "frame-ancestors "+fa)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeJSONError(w, http.StatusBadGateway, "guest_unreachable")
		},
	}
	return mw
}

// internalMachineHeader carries the resolved machine id from ServeHTTP to the
// proxy Director without re-parsing the host. It is stripped before the upstream
// request is sent.
const internalMachineHeader = "X-Proteos-Machine-Internal"

// Matches reports whether host is a machine-web host this handler should serve.
func (mw *MachineWeb) Matches(host string) bool {
	if mw == nil {
		return false
	}
	_, ok := mw.parseHost(host)
	return ok
}

// parseHost extracts the machine id from a m-<uuid>.<domain> host (any port
// stripped, case-insensitive). ok=false for anything else.
func (mw *MachineWeb) parseHost(host string) (machineID string, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + strings.ToLower(mw.cfg.Domain)
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	m := machineLabelRe.FindStringSubmatch(label)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ServeHTTP handles one machine-web request: the auth handler at /__proteos/auth,
// otherwise the authenticated code-server proxy.
func (mw *MachineWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	machineID, ok := mw.parseHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/__proteos/auth" {
		mw.handleAuth(w, r, machineID)
		return
	}
	mw.handleProxy(w, r, machineID)
}

// handleAuth validates the one-shot web-session token and sets the subdomain
// cookie, then redirects to the editor root.
func (mw *MachineWeb) handleAuth(w http.ResponseWriter, r *http.Request, machineID string) {
	tok, err := verifyMachineToken(mw.cfg.SigningKey, r.URL.Query().Get("token"))
	if err != nil || tok.MachineID != machineID {
		writeJSONError(w, http.StatusForbidden, "bad_token")
		return
	}
	cookieVal := signMachineCookie(mw.cfg.SigningKey, machineCookie{
		MachineID: tok.MachineID,
		SessionID: tok.SessionID,
		Exp:       time.Now().Add(machineCookieTTL).Unix(),
	})
	http.SetCookie(w, &http.Cookie{
		Name:        MachineCookieName,
		Value:       cookieVal,
		Path:        "/",
		HttpOnly:    true,
		Secure:      mw.cfg.CookieSecure,
		SameSite:    http.SameSiteNoneMode, // cross-origin iframe needs SameSite=None
		Partitioned: true,                  // CHIPS: partitioned per top-level site
		Expires:     time.Now().Add(machineCookieTTL),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleProxy re-validates the cookie, parent session, and ownership on every
// request, then proxies to code-server. WebSocket upgrades additionally enforce
// the subdomain Origin and register for revocation.
func (mw *MachineWeb) handleProxy(w http.ResponseWriter, r *http.Request, machineID string) {
	c, err := r.Cookie(MachineCookieName)
	if err != nil || c.Value == "" {
		writeJSONError(w, http.StatusUnauthorized, "no_cookie")
		return
	}
	cookie, err := verifyMachineCookie(mw.cfg.SigningKey, c.Value)
	if err != nil || cookie.MachineID != machineID {
		writeJSONError(w, http.StatusUnauthorized, "bad_cookie")
		return
	}

	owner, ok, err := mw.cfg.Sessions.SessionOwner(r.Context(), cookie.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !ok {
		// Parent session revoked/expired ⇒ the editor cookie is dead too.
		writeJSONError(w, http.StatusForbidden, "session_revoked")
		return
	}

	mOwner, running, exists, err := mw.cfg.Machines.MachineOwner(r.Context(), machineID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !exists || mOwner != owner {
		writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}
	if !running {
		writeJSONError(w, http.StatusBadGateway, "machine_stopped")
		return
	}

	// WebSocket upgrades: the Origin must be this exact subdomain (CSWSH defense
	// against code-server's own sockets), and the live socket joins the revocation
	// registry under the parent session id so logout closes it.
	if isWebSocketUpgrade(r) {
		if r.Header.Get("Origin") != mw.subdomainOrigin(r) {
			writeJSONError(w, http.StatusForbidden, "origin_forbidden")
			return
		}
		w = &revocableRW{ResponseWriter: w, sessionID: cookie.SessionID, registry: mw.cfg.Registry}
	}

	r.Header.Set(internalMachineHeader, machineID)
	mw.proxy.ServeHTTP(w, r)
}

// subdomainOrigin reconstructs this request's own origin (scheme://host) for the
// WebSocket Origin check.
func (mw *MachineWeb) subdomainOrigin(r *http.Request) string {
	return forwardedProto(r) + "://" + r.Host
}

// forwardedProto resolves the external scheme, honouring the proxy layer's
// X-Forwarded-Proto (the app-stack nginx / NPMplus terminate TLS).
func forwardedProto(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// isWebSocketUpgrade reports whether r is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		tokenInHeader(r.Header, "Connection", "upgrade")
}

func tokenInHeader(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// revocableRW wraps a ResponseWriter so that a hijacked (upgraded) connection is
// registered in the revocation registry under the parent session id. Closing the
// connection — by the proxy at end-of-life or by a revoke — unregisters it.
type revocableRW struct {
	http.ResponseWriter
	sessionID string
	registry  *Registry
}

func (rw *revocableRW) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("gateway: response writer is not a hijacker")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	rc := &registeredConn{Conn: conn}
	rc.unreg = rw.registry.Register(rw.sessionID, func() { _ = rc.Close() })
	return rc, brw, nil
}

// registeredConn unregisters from the revocation registry exactly once, whether
// the proxy closes it normally or a revoke does. A revoke closes the raw tunnel,
// which the browser observes as the editor socket dropping (the code-server
// equivalent of the terminal's 4001 close — we deliberately do not parse the WS
// to inject a code frame).
type registeredConn struct {
	net.Conn
	unreg func()
	once  sync.Once
}

func (c *registeredConn) Close() error {
	c.once.Do(func() {
		if c.unreg != nil {
			c.unreg()
		}
	})
	return c.Conn.Close()
}

// MintWebSessionURL builds the m-<uuid>.<domain>/__proteos/auth?token=… URL the
// SPA navigates the editor frame to. scheme is the external scheme of the main
// origin. It is the gateway's half of POST /api/machine/web-session.
func (mw *MachineWeb) MintWebSessionURL(scheme, machineID, sessionID, userID string) string {
	tok := signMachineToken(mw.cfg.SigningKey, machineToken{
		MachineID: machineID,
		UserID:    userID,
		SessionID: sessionID,
		Exp:       time.Now().Add(webTokenTTL).Unix(),
	})
	u := url.URL{
		Scheme:   scheme,
		Host:     "m-" + machineID + "." + mw.cfg.Domain,
		Path:     "/__proteos/auth",
		RawQuery: url.Values{"token": {tok}}.Encode(),
	}
	return u.String()
}

// Domain returns the configured machine domain (for the SPA/config surface).
func (mw *MachineWeb) Domain() string {
	if mw == nil {
		return ""
	}
	return mw.cfg.Domain
}
