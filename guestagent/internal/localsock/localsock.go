// Package localsock is the in-VM unix-socket bridge between the git credential
// helper subprocess and the long-running guest agent (Phase 7 decision #5). git
// invokes `guestagent git-credential`, which connects to AgentSockPath and asks
// for a credential; the agent relays the request over the control channel and
// returns the result. Keeping the helper a thin socket client means no WebSocket
// client lives in a short-lived subprocess, and nothing token-shaped is written
// to disk: the socket is on tmpfs and the credential only ever transits memory.
package localsock

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	guestwire "github.com/tavon/proteos/guestagent/api"
)

// Request is the helper → agent credential lookup (one JSON object per conn).
type Request struct {
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
}

// Response is the agent → helper reply. On success Username/Password are set and
// Error is empty; on failure Error carries a guestwire.ErrCode* value.
type Response struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Expiry   string `json:"expiry,omitempty"` // RFC3339, mirrors the channel response
	Error    string `json:"error,omitempty"`
}

// Resolver fetches a credential, typically *ctlchan.Manager.
type Resolver interface {
	Credential(ctx context.Context, host, protocol string) (guestwire.GitCredentialResponse, error)
}

// Server serves credential lookups on a unix socket.
type Server struct {
	path     string
	resolver Resolver
}

// New builds a Server bound to path (typically guestwire.AgentSockPath).
func New(path string, r Resolver) *Server {
	return &Server{path: path, resolver: r}
}

// Serve listens on the unix socket until ctx is cancelled. The socket directory
// is created 0700 and the socket itself 0600 — only the in-VM root user (which
// runs both git and the agent) can reach it.
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.path) // clear a stale socket from a prior boot
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	slog.Info("credential helper socket listening", "path", s.path)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	var req Request
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		writeResp(c, Response{Error: guestwire.ErrCodeUnavailable})
		return
	}
	cred, err := s.resolver.Credential(ctx, req.Host, req.Protocol)
	if err != nil {
		writeResp(c, Response{Error: errorCode(err)})
		return
	}
	writeResp(c, Response{Username: cred.Username, Password: cred.Password, Expiry: cred.Expiry})
}

func writeResp(c net.Conn, r Response) {
	if err := json.NewEncoder(c).Encode(r); err != nil {
		slog.Debug("credential socket: write reply failed", "err", err)
	}
}

// errorCode extracts a guestwire.ErrCode* string from a resolver error,
// defaulting to unavailable.
func errorCode(err error) string {
	type coded interface{ ErrorCode() string }
	var c coded
	if errors.As(err, &c) {
		return c.ErrorCode()
	}
	return guestwire.ErrCodeUnavailable
}

// Fetch is the helper-side client: it dials path, sends req, and returns the
// agent's reply. Used by the `guestagent git-credential` subcommand.
func Fetch(path string, req Request) (Response, error) {
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
