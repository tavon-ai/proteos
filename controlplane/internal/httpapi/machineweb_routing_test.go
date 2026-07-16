package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
)

const routeMachineID = "11111111-1111-1111-1111-111111111111"

type rtGuests struct{}

func (rtGuests) DialGuest(context.Context, string, uint32) (net.Conn, error) {
	return nil, context.Canceled // never reached in these dispatch tests
}

type rtSessions struct{}

func (rtSessions) SessionOwner(context.Context, string) (string, bool, error) { return "", false, nil }

type rtMachines struct{}

func (rtMachines) MachineOwner(context.Context, string) (string, bool, bool, error) {
	return "", false, false, nil
}

// TestHostFirstRouting asserts the structural origin split (decision #1): a
// machine subdomain is served ONLY by the machine-web handler (never the main
// mux), and any other host goes to the main mux.
func TestHostFirstRouting(t *testing.T) {
	t.Parallel()
	mw := gateway.NewMachineWeb(gateway.MachineWebConfig{
		Domain:     "localhost",
		SigningKey: []byte("k"),
		Guests:     rtGuests{},
		Registry:   gateway.NewRegistry(),
		Sessions:   rtSessions{},
		Machines:   rtMachines{},
	})
	if mw == nil {
		t.Fatal("expected a machine-web handler")
	}
	srv := &Server{MachineWeb: mw}

	mainHit := false
	h := srv.hostRouter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mainHit = true
		w.WriteHeader(http.StatusOK)
	}))

	// Subdomain host: main mux must NOT be hit; the machine-web handler answers
	// (401, no cookie) — never the SPA/API.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Host = "m-" + routeMachineID + ".localhost"
	h.ServeHTTP(rec, req)
	if mainHit {
		t.Error("main mux was reached for a machine subdomain host")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("subdomain /api/me = %d, want 401 (machine-web, no cookie)", rec.Code)
	}

	// Main host: dispatched to the main mux.
	mainHit = false
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req2.Host = "app.localhost"
	h.ServeHTTP(rec2, req2)
	if !mainHit {
		t.Error("main mux was NOT reached for the main host")
	}
}
