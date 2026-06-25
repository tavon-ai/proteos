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
	"strconv"
	"strings"
	"sync"
	"time"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
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

// machineLabelRe matches the m-<uuid> editor host label and the PP1 preview form
// m-<uuid>-p<port> (decision #1). The full UUID keeps the label valid and not
// trivially guessable; authz (not obscurity) is load-bearing. The optional
// -p<port> group (1–5 digits) selects a preview port; absent ⇒ the port-less
// editor origin. The label stays ≤63 chars (m- + 36 + -p + ≤5 ≈ 45).
var machineLabelRe = regexp.MustCompile(`^m-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:-p([0-9]{1,5}))?$`)

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

	// PreviewPortMin / PreviewPortMax bound the previewable application ports the
	// mint will issue a token for (PP2). Reserved system ports 1024/1025 stay
	// rejected regardless. Zero ⇒ the default high range
	// (agentapi.DefaultPreviewPortMin/Max). These mirror the node-agent's
	// PROTEOS_PREVIEW_PORT_MIN/MAX so the mint and the allowlist agree.
	PreviewPortMin uint32
	PreviewPortMax uint32
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
	if cfg.PreviewPortMin == 0 {
		cfg.PreviewPortMin = agentapi.DefaultPreviewPortMin
	}
	if cfg.PreviewPortMax == 0 {
		cfg.PreviewPortMax = agentapi.DefaultPreviewPortMax
	}
	mw := &MachineWeb{cfg: cfg}

	transport := &http.Transport{
		// Each connection is a fresh guest tunnel to a (machine, guest-port) pair
		// encoded in the dial address (the Director sets req.URL.Host =
		// machineID:guestPort). Pooling keys on that host:port, so distinct
		// machines — and the editor (web port) vs each preview app port of the same
		// machine — never share a tunnel.
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				host, portStr = addr, ""
			}
			port := agentapi.GuestWebPort
			if p, err := strconv.ParseUint(portStr, 10, 32); err == nil && p != 0 {
				port = uint32(p)
			}
			return cfg.Guests.DialGuest(ctx, host, port)
		},
		MaxIdleConnsPerHost:   4,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	mw.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Director: func(r *http.Request) {
			machineID := r.Header.Get(internalMachineHeader)
			guestPort := r.Header.Get(internalPortHeader)
			r.Header.Del(internalMachineHeader)
			r.Header.Del(internalPortHeader)
			// Dial/cache key is machine id + guest port; the Host header the backend
			// sees stays the original subdomain so its Origin==Host WebSocket check
			// passes (the historically fragile part of proxying code-server).
			r.URL.Scheme = "http"
			r.URL.Host = net.JoinHostPort(machineID, guestPort)
			r.Header.Set("X-Forwarded-Host", r.Host)
			r.Header.Set("X-Forwarded-Proto", forwardedProto(r))
		},
		ModifyResponse: func(resp *http.Response) error {
			// Framing policy is expressed ONLY via CSP frame-ancestors (decision
			// #3): the main SPA origin may embed the editor. A legacy
			// X-Frame-Options header (code-server can emit one) is all-or-nothing
			// and would override the CSP to block the iframe, so strip it and let
			// frame-ancestors govern. (A proxy layer that re-adds X-Frame-Options
			// — e.g. NPMplus — must be configured to drop it; see RUNBOOK Part G.)
			resp.Header.Del("X-Frame-Options")
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

// internalMachineHeader / internalPortHeader carry the resolved machine id and
// guest port from handleProxy to the proxy Director without re-parsing the host.
// Both are stripped before the upstream request is sent.
const (
	internalMachineHeader = "X-Proteos-Machine-Internal"
	internalPortHeader    = "X-Proteos-Port-Internal"
)

// Matches reports whether host is a machine-web host this handler should serve.
func (mw *MachineWeb) Matches(host string) bool {
	if mw == nil {
		return false
	}
	_, _, ok := mw.parseHost(host)
	return ok
}

// parseHost extracts the machine id and preview port from a machine-web host
// (any URL port stripped, case-insensitive). For the editor host m-<uuid>.<domain>
// the returned port is 0; for the preview host m-<uuid>-p<port>.<domain> it is
// the parsed port. ok=false for anything else (including a -p group that does not
// parse as a uint16-range port).
func (mw *MachineWeb) parseHost(host string) (machineID string, port uint32, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + strings.ToLower(mw.cfg.Domain)
	if !strings.HasSuffix(host, suffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(host, suffix)
	m := machineLabelRe.FindStringSubmatch(label)
	if m == nil {
		return "", 0, false
	}
	if m[2] != "" {
		p, err := strconv.ParseUint(m[2], 10, 32)
		if err != nil || p == 0 || p > 65535 {
			return "", 0, false
		}
		return m[1], uint32(p), true
	}
	return m[1], 0, true
}

// ServeHTTP handles one machine-web request: the auth handler at /__proteos/auth,
// otherwise the authenticated code-server proxy.
func (mw *MachineWeb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	machineID, port, ok := mw.parseHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/__proteos/auth" {
		mw.handleAuth(w, r, machineID, port)
		return
	}
	mw.handleProxy(w, r, machineID, port)
}

// handleAuth validates the one-shot web-session token and sets the subdomain
// cookie, then redirects to the origin root. The token must match both the
// machine and the preview port of this exact origin (decision #2/#3): a token
// minted for one (machine, port) cannot set a cookie on another.
func (mw *MachineWeb) handleAuth(w http.ResponseWriter, r *http.Request, machineID string, port uint32) {
	tok, err := verifyMachineToken(mw.cfg.SigningKey, r.URL.Query().Get("token"))
	if err != nil || tok.MachineID != machineID || tok.Port != port {
		writeJSONError(w, http.StatusForbidden, "bad_token")
		return
	}
	cookieVal := signMachineCookie(mw.cfg.SigningKey, machineCookie{
		MachineID: tok.MachineID,
		SessionID: tok.SessionID,
		Exp:       time.Now().Add(machineCookieTTL).Unix(),
		Port:      tok.Port,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     MachineCookieName,
		Value:    cookieVal,
		Path:     "/",
		HttpOnly: true,
		// SameSite=None + Partitioned (CHIPS) is what lets the embedded
		// cross-origin editor iframe carry this cookie — and BOTH mandate Secure:
		// browsers SILENTLY DROP a SameSite=None or Partitioned cookie that isn't
		// Secure, which then 401s every editor request. The editor is always
		// reached over HTTPS (TLS terminates at the proxy), so Secure is
		// unconditional here, independent of cfg.CookieSecure (which governs the
		// main host-only Lax session cookie, where Secure is optional).
		Secure:      true,
		SameSite:    http.SameSiteNoneMode,
		Partitioned: true,
		Expires:     time.Now().Add(machineCookieTTL),
	})
	// Phase 9: open code-server directly on the project folder when the token
	// carries one (decision #5). The folder was validated against the listable
	// project set at mint time; re-check the /workspace prefix as cheap defence
	// before reflecting it into the redirect.
	target := "/"
	if tok.Folder != "" {
		if clean, ok := guestwire.CleanWorkdir(tok.Folder); ok {
			target = "/?" + url.Values{"folder": {clean}}.Encode()
		}
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleProxy re-validates the cookie, parent session, and ownership on every
// request, then proxies to the guest backend — code-server for the editor
// (port 0) or the user's app on the preview port. WebSocket upgrades
// additionally enforce the subdomain Origin and register for revocation.
func (mw *MachineWeb) handleProxy(w http.ResponseWriter, r *http.Request, machineID string, port uint32) {
	c, err := r.Cookie(MachineCookieName)
	if err != nil || c.Value == "" {
		writeJSONError(w, http.StatusUnauthorized, "no_cookie")
		return
	}
	cookie, err := verifyMachineCookie(mw.cfg.SigningKey, c.Value)
	if err != nil || cookie.MachineID != machineID || cookie.Port != port {
		// The port mismatch is what stops a cookie minted for one preview origin
		// (or the editor) from being replayed against another (machine, port).
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

	// Resolve the guest port to dial: the editor (port 0) reaches code-server on
	// the web port; a preview reaches the user's app port through the forwarder.
	guestPort := agentapi.GuestWebPort
	if port != 0 {
		guestPort = port
	}
	r.Header.Set(internalMachineHeader, machineID)
	r.Header.Set(internalPortHeader, strconv.FormatUint(uint64(guestPort), 10))
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

// MintWebSessionURL builds the <host>/__proteos/auth?token=… URL the SPA
// navigates the editor/preview frame to. scheme is the external scheme of the
// main origin. A zero port mints the editor origin (m-<uuid>); a non-zero port
// mints the preview origin (m-<uuid>-p<port>) and binds the port into the token,
// so the cookie set at /__proteos/auth is scoped to that single (machine, port)
// origin. It is the gateway's half of POST /api/machine/web-session.
func (mw *MachineWeb) MintWebSessionURL(scheme, machineID, sessionID, userID, folder string, port uint32) string {
	tok := signMachineToken(mw.cfg.SigningKey, machineToken{
		MachineID: machineID,
		UserID:    userID,
		SessionID: sessionID,
		Exp:       time.Now().Add(webTokenTTL).Unix(),
		Folder:    folder,
		Port:      port,
	})
	label := "m-" + machineID
	if port != 0 {
		label += "-p" + strconv.FormatUint(uint64(port), 10)
	}
	u := url.URL{
		Scheme:   scheme,
		Host:     label + "." + mw.cfg.Domain,
		Path:     "/__proteos/auth",
		RawQuery: url.Values{"token": {tok}}.Encode(),
	}
	return u.String()
}

// ValidPreviewPort reports whether port is an admissible preview application
// port under the configured range (PP2): in [PreviewPortMin, PreviewPortMax] and
// not a reserved system port (1024/1025). The mint rejects everything else with
// a 400 before issuing a token.
func (mw *MachineWeb) ValidPreviewPort(port uint32) bool {
	return agentapi.ValidPreviewPort(port, mw.cfg.PreviewPortMin, mw.cfg.PreviewPortMax)
}

// Domain returns the configured machine domain (for the SPA/config surface).
func (mw *MachineWeb) Domain() string {
	if mw == nil {
		return ""
	}
	return mw.cfg.Domain
}
