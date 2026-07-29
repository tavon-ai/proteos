package auth_test

import (
	"net/http"
	"testing"
)

// meWithBearer calls an authenticated endpoint with an Authorization: Bearer
// header and no cookie — the shape a sibling suite service uses.
func meWithBearer(t *testing.T, h *harness, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	// A bare client: the harness client carries a cookie jar, and this path
	// must work with no browser session at all.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// An access token from the IdP ProteOS logs in with authenticates the API, so
// the harness can call as the user who is signed in there. On first contact the
// user row is created by the same rules as a browser login.
func TestIdPBearerAuthenticatesAndProvisionsUser(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	resp := meWithBearer(t, h, "zat_access")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me with an IdP bearer = %d, want 200", resp.StatusCode)
	}
	// Resolution went through resolveOIDCUser, so the row is keyed by subject.
	if id := userIDByOIDCSub(t, h, "sub-1"); !id.Valid {
		t.Error("no user row was created for the bearer's subject")
	}
}

// A token the IdP will not answer for is rejected, and rejection is a flat 401
// — never a fallback to any other credential.
func TestIdPBearerRejectedWhenIdPRefusesIt(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	resp := meWithBearer(t, h, "not-a-real-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/me with an unknown bearer = %d, want 401", resp.StatusCode)
	}
}

// The resolved identity is cached, so a chatty caller does not put one userinfo
// round trip on the IdP per API call.
func TestIdPBearerResolutionIsCached(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	for range 3 {
		resp := meWithBearer(t, h, "zat_access")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if got := idp.userInfoCalls.Load(); got != 1 {
		t.Errorf("userinfo called %d times across 3 requests, want 1", got)
	}
}

// A rejected token must not be cached: it would turn a transient IdP refusal
// into a sticky one, and it would let unbounded junk fill the cache.
func TestRejectedIdPBearerIsNotCached(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	for range 2 {
		resp := meWithBearer(t, h, "wrong")
		resp.Body.Close()
	}
	if got := idp.userInfoCalls.Load(); got != 2 {
		t.Errorf("userinfo called %d times for 2 rejected tokens, want 2", got)
	}
}

// A PAT-shaped credential keeps routing to the PAT validator. This deployment
// has no PAT manager wired, so it must fail closed rather than fall through to
// the IdP — where it would be an unknown token anyway, but for the wrong
// reason, and after a pointless round trip.
func TestPATShapedBearerNeverReachesTheIdP(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	resp := meWithBearer(t, h, "proteos_pat_bogus")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PAT-shaped bearer = %d, want 401", resp.StatusCode)
	}
	if got := idp.userInfoCalls.Load(); got != 0 {
		t.Errorf("userinfo called %d times for a PAT-shaped bearer, want 0", got)
	}
}

// Bearer auth stays exempt from the CSRF header: it is not an ambient browser
// credential, and a service caller has no way to know about X-Requested-By.
func TestIdPBearerExemptFromCSRFHeader(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.liveAccessToken = "zat_access"
	h := newHarness(t, idp, newFakeGitHub(t), nil)

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer zat_access")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("bearer-authenticated POST was refused for a missing CSRF header")
	}
}
