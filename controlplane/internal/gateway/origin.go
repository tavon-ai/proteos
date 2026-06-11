package gateway

import (
	"net/http"
	"slices"
)

// AllowsOrigin enforces the WebSocket Origin check. A browser cannot set the
// custom X-Requested-By CSRF header on a WebSocket upgrade, so an exact-match
// Origin check is the WS-equivalent of CSRF protection (Phase 3 decision #8):
// the Origin header must be present and exactly equal one of the configured
// allowed origins. Wildcards and substring matches are deliberately rejected.
// It is exported so the route handler can reject a bad Origin (403) before
// touching the machine — keeping the auth → origin → resolve → dial order.
func (p *Proxy) AllowsOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return slices.Contains(p.allowedOrigins, origin)
}
