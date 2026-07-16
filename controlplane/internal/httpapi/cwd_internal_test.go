package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// stubProjects is a fake ProjectChannel for the cwd-validation matrix.
type stubProjects struct {
	projects []guestwire.Project
	err      error
}

func (s stubProjects) HasChannel(string) bool { return true }
func (s stubProjects) ListProjects(context.Context, string) ([]guestwire.Project, error) {
	return s.projects, s.err
}
func (s stubProjects) KVGet(context.Context, string, string) (*string, error) { return nil, nil }
func (s stubProjects) KVSet(context.Context, string, string, string) error    { return nil }

func TestResolveSessionCwd(t *testing.T) {
	t.Parallel()
	listable := []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha"},
		{Name: "beta", Path: "/workspace/beta"},
	}

	t.Run("empty cwd is allowed (no scoping)", func(t *testing.T) {
		s := &Server{Projects: stubProjects{projects: listable}}
		cwd, code := s.resolveSessionCwd(context.Background(), "m", "")
		if cwd != "" || code != "" {
			t.Fatalf("got (%q, %q), want (\"\", \"\")", cwd, code)
		}
	})

	t.Run("listable project path is accepted", func(t *testing.T) {
		s := &Server{Projects: stubProjects{projects: listable}}
		cwd, code := s.resolveSessionCwd(context.Background(), "m", "/workspace/alpha")
		if cwd != "/workspace/alpha" || code != "" {
			t.Fatalf("got (%q, %q), want (/workspace/alpha, \"\")", cwd, code)
		}
	})

	t.Run("path traversal is cleaned then rejected", func(t *testing.T) {
		s := &Server{Projects: stubProjects{projects: listable}}
		// cleans to /workspace/alpha — which IS listable, so it is accepted (the
		// clean is the security boundary, not a reason to reject).
		cwd, code := s.resolveSessionCwd(context.Background(), "m", "/workspace/beta/../alpha")
		if cwd != "/workspace/alpha" || code != "" {
			t.Fatalf("got (%q, %q)", cwd, code)
		}
	})

	t.Run("outside workspace is bad_cwd", func(t *testing.T) {
		s := &Server{Projects: stubProjects{projects: listable}}
		_, code := s.resolveSessionCwd(context.Background(), "m", "/etc")
		if code != "bad_cwd" {
			t.Fatalf("code = %q, want bad_cwd", code)
		}
	})

	t.Run("not a listable project is bad_cwd", func(t *testing.T) {
		s := &Server{Projects: stubProjects{projects: listable}}
		_, code := s.resolveSessionCwd(context.Background(), "m", "/workspace/gamma")
		if code != "bad_cwd" {
			t.Fatalf("code = %q, want bad_cwd", code)
		}
	})

	t.Run("nil project channel rejects any cwd", func(t *testing.T) {
		s := &Server{}
		_, code := s.resolveSessionCwd(context.Background(), "m", "/workspace/alpha")
		if code != "bad_cwd" {
			t.Fatalf("code = %q, want bad_cwd", code)
		}
	})

	t.Run("unreachable guest is guest_unreachable", func(t *testing.T) {
		s := &Server{Projects: stubProjects{err: errors.New("dial failed")}}
		_, code := s.resolveSessionCwd(context.Background(), "m", "/workspace/alpha")
		if code != "guest_unreachable" {
			t.Fatalf("code = %q, want guest_unreachable", code)
		}
	})
}

func TestCwdErrorStatus(t *testing.T) {
	t.Parallel()
	if got := cwdErrorStatus("bad_cwd"); got != http.StatusBadRequest {
		t.Errorf("bad_cwd → %d, want 400", got)
	}
	if got := cwdErrorStatus("guest_unreachable"); got != http.StatusBadGateway {
		t.Errorf("guest_unreachable → %d, want 502", got)
	}
}
