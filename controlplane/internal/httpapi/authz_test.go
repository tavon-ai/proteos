package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
)

// Every protected route must reject unauthenticated requests with 401 — the
// guard is registered on the prefix so Phase 2's real handlers inherit it. No
// DB is needed: requireAuth returns 401 before touching the session manager
// when there is no cookie.
func TestProtectedRoutesRejectUnauthenticated(t *testing.T) {
	srv := &httpapi.Server{
		Sessions: session.NewManager(store.New(nil), time.Hour),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/machine"},
		{http.MethodPost, "/api/machine"},
		{http.MethodPost, "/api/machine/start"},
		{http.MethodPost, "/api/machine/stop"},
	}

	client := &http.Client{}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			// Even with the CSRF header present, no session ⇒ 401.
			req.Header.Set("X-Requested-By", "proteos")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", resp.StatusCode)
			}
		})
	}
}

func TestHealthzIsPublic(t *testing.T) {
	srv := &httpapi.Server{Sessions: session.NewManager(store.New(nil), time.Hour)}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz want 200, got %d", resp.StatusCode)
	}
}
