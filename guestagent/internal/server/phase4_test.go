package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	guestwire "github.com/tavon/proteos/guestagent/api"
	"github.com/tavon/proteos/guestagent/internal/persist"
	"github.com/tavon/proteos/guestagent/internal/term"
)

// putJSON issues an authenticated-free PUT with a JSON body.
func putJSON(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func newP4Server(t *testing.T, p Persister) *httptest.Server {
	t.Helper()
	mgr := term.NewManager(term.Config{Shell: "/bin/sh", ScrollbackKiB: 64})
	t.Cleanup(mgr.Shutdown)
	ts := httptest.NewServer(New(mgr, p, nil, nil).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestResumeAndInfo drives PUT /resume and GET /info against a real dir-mode
// persist. /resume records a resumed boot (clock/entropy are no-ops off Linux),
// and /info then reports it.
func TestResumeAndInfo(t *testing.T) {
	p, err := persist.Setup(persist.Config{Dir: t.TempDir(), Version: "srv-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ts := newP4Server(t, p)

	body, _ := json.Marshal(guestwire.ResumeRequest{
		UnixNanos:  0,
		EntropyB64: base64.StdEncoding.EncodeToString(make([]byte, 32)),
	})
	resp := putJSON(t, ts.URL+"/resume", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume: status %d", resp.StatusCode)
	}
	var rr guestwire.ResumeResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode resume resp: %v", err)
	}

	// GET /info reflects the resumed boot.
	iresp, err := http.Get(ts.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	defer iresp.Body.Close()
	var info guestwire.Info
	if err := json.NewDecoder(iresp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Persist != guestwire.PersistDir {
		t.Fatalf("info persist=%q, want dir", info.Persist)
	}
	if info.LastBoot == nil || info.LastBoot.Kind != guestwire.BootResumed {
		t.Fatalf("info last boot=%+v, want resumed", info.LastBoot)
	}
}

// TestResumeDisabledWhenNoPersist proves the routes report 503 when persistence
// is disabled (nil persister) — but terminals still work (degraded-friendly).
func TestResumeDisabledWhenNoPersist(t *testing.T) {
	ts := newP4Server(t, nil)
	resp := putJSON(t, ts.URL+"/resume", []byte(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("resume w/o persist: status %d, want 503", resp.StatusCode)
	}
}
