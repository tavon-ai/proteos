package nodeclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("http://agent:9090/", "tok")
	if c.BaseURL() != "http://agent:9090" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL())
	}
}

func TestNewPinnedEmptyCAFileBehavesLikeNew(t *testing.T) {
	c, err := NewPinned("http://agent:9090", "tok", "")
	if err != nil {
		t.Fatalf("NewPinned: %v", err)
	}
	if c.tlsCfg != nil {
		t.Error("empty caFile should leave tlsCfg nil")
	}
}

func TestNewPinnedMissingFile(t *testing.T) {
	_, err := NewPinned("http://agent:9090", "tok", filepath.Join(t.TempDir(), "nope.pem"))
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestNewPinnedBadPEM(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(f, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewPinned("http://agent:9090", "tok", f)
	if err == nil || !strings.Contains(err.Error(), "no usable certificates") {
		t.Fatalf("want 'no usable certificates' error, got %v", err)
	}
}

// newServer returns a client pointed at a test server whose handler asserts the
// bearer token and dispatches on method+path.
func newServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "secret-token")
}

func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get(agentapi.AuthHeader); got != agentapi.BearerPrefix+"secret-token" {
		t.Errorf("auth header = %q", got)
	}
}

func TestEnsure(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != http.MethodPut || r.URL.Path != "/v1/machines/m1" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"handle":"h-123"}`)
	})
	resp, err := c.Ensure(context.Background(), "m1", agentapi.EnsureRequest{Vcpus: 2})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resp.Handle != "h-123" {
		t.Errorf("handle = %q", resp.Handle)
	}
}

func TestStopWithMode(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/m1/stop" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json for non-empty body", ct)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := c.Stop(context.Background(), "m1", "poweroff"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopEmptyModeSendsNoBody(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			t.Errorf("content-type = %q, want empty for nil body", ct)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := c.Stop(context.Background(), "m1", ""); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStatus(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"machine_id":"m1","state":"running"}`)
	})
	st, err := c.Status(context.Background(), "m1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != "running" || st.MachineID != "m1" {
		t.Errorf("status = %+v", st)
	}
}

func TestStatusUnknownMachine(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown_machine"}`)
	})
	_, err := c.Status(context.Background(), "ghost")
	if !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("want ErrUnknownMachine, got %v", err)
	}
}

func TestList(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"machines":[{"machine_id":"a"},{"machine_id":"b"}]}`)
	})
	ms, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ms) != 2 || ms[0].MachineID != "a" || ms[1].MachineID != "b" {
		t.Errorf("machines = %+v", ms)
	}
}

func TestDestroy(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Destroy(context.Background(), "m1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestHealth(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestDoUnexpectedStatus(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	err := c.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agent returned 500") {
		t.Fatalf("want unexpected-status error, got %v", err)
	}
}

func TestDoBadJSON(t *testing.T) {
	c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not json`)
	})
	_, err := c.Status(context.Background(), "m1")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestDoNetworkError(t *testing.T) {
	// Point at a server that is immediately closed so the dial fails.
	srv := httptest.NewServer(http.NotFoundHandler())
	c := New(srv.URL, "tok")
	srv.Close()
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected network error after server close")
	}
}

func TestDialGuestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.URL.Path != "/v1/machines/m1/guest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get(agentapi.GuestPortParam); got != "1024" {
			t.Errorf("port param = %q, want 1024", got)
		}
		if up := r.Header.Get("Upgrade"); up != agentapi.UpgradeGuestProto {
			t.Errorf("upgrade header = %q", up)
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		// Switch protocols, then send a payload the client must surface via the
		// buffered conn.
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + agentapi.UpgradeGuestProto + "\r\nConnection: Upgrade\r\n\r\nHELLO")
		_ = buf.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-token")
	conn, err := c.DialGuest(context.Background(), "m1", agentapi.GuestTerminalPort)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer conn.Close()

	got, _ := bufio.NewReader(conn).ReadString('O')
	if got != "HELLO" {
		t.Errorf("buffered bytes = %q, want HELLO", got)
	}
}

func TestDialGuestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrUnknownMachine},
		{http.StatusConflict, ErrNotRunning},
		{http.StatusBadGateway, ErrGuestUnreachable},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := New(srv.URL, "tok")
		_, err := c.DialGuest(context.Background(), "m1", 0)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d => %v, want %v", tc.status, err, tc.want)
		}
		srv.Close()
	}
}

func TestDialGuestUnsupportedScheme(t *testing.T) {
	c := New("ftp://agent", "tok")
	_, err := c.DialGuest(context.Background(), "m1", 0)
	if err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("want unsupported-scheme error, got %v", err)
	}
}
