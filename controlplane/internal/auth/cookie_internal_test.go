package auth

import (
	"net/http"
	"testing"
	"time"
)

// TestSessionCookieIsHostOnly is the Phase 8 origin-isolation regression (task
// 8.0b): the main session cookie MUST stay host-only — no Domain attribute — so
// it is never sent to a machine subdomain (m-<uuid>.<domain>). This is the
// precondition the whole web-origin-isolation decision rests on (fact #1): the
// editor origin can therefore never receive the main session cookie, and uses
// its own partitioned proteos_machine cookie instead. If someone ever adds a
// Domain (e.g. ".proteos.example" to share auth across subdomains), this fails.
func TestSessionCookieIsHostOnly(t *testing.T) {
	h := &Handler{cfg: Config{CookieSecure: true, SessionTTL: time.Hour}}

	set := h.sessionCookie("opaque-token")
	if set.Name != SessionCookieName {
		t.Fatalf("session cookie name = %q, want %q", set.Name, SessionCookieName)
	}
	if set.Domain != "" {
		t.Errorf("session cookie has Domain=%q; it MUST be host-only (empty) so it never reaches m-*.<domain> subdomains", set.Domain)
	}
	if !set.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if set.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %d, want Lax", set.SameSite)
	}
	if set.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", set.Path)
	}

	// The clearing cookie must also stay host-only — a Domain on the clear would
	// imply a Domain on the set.
	clear := h.clearSessionCookie()
	if clear.Domain != "" {
		t.Errorf("clear session cookie has Domain=%q; must be host-only", clear.Domain)
	}
}
