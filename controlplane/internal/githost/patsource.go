// Package githost resolves a user's stored credential for one of the
// additional git hosts on the PROTEOS_GIT_PUBLIC_HOSTS allowlist (Gitea/
// Forgejo phase 2). It is deliberately tiny — PATs neither expire nor rotate,
// so unlike the GitHub TokenSource there is no refresh flow, no expiry skew,
// and no per-user lock: just a link-row lookup plus a secret read. A revoked
// PAT surfaces as a 401 from the host at use time.
package githost

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// ErrNoLink means the user has not stored a PAT for the host. Callers map it
// to forbidden_host (credential broker) or githost_token_required (PR API).
var ErrNoLink = errors.New("githost: no credential stored for host")

// PATSource reads per-(user, host) PATs written by the /api/git/hosts API.
type PATSource struct {
	q       *store.Queries
	secrets secrets.Store
}

// NewPATSource wires a PATSource.
func NewPATSource(q *store.Queries, sec secrets.Store) *PATSource {
	return &PATSource{q: q, secrets: sec}
}

// HostCredential returns the Basic-auth pair for git-over-https on host: the
// user's login on that host as username and the PAT as password. host is
// matched in its lowercased host[:port] allowlist form.
func (s *PATSource) HostCredential(ctx context.Context, userID, host string) (username, password string, err error) {
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return "", "", fmt.Errorf("bad user id: %w", err)
	}
	link, err := s.q.GetGitHostLink(ctx, store.GetGitHostLinkParams{UserID: uid, Host: strings.ToLower(host)})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNoLink
	}
	if err != nil {
		return "", "", fmt.Errorf("load git host link: %w", err)
	}
	data, err := s.secrets.Get(link.SecretRef)
	if errors.Is(err, secrets.ErrNotFound) {
		return "", "", ErrNoLink
	}
	if err != nil {
		return "", "", fmt.Errorf("read git host secret: %w", err)
	}
	token, login := data[secrets.GitHostFieldToken], data[secrets.GitHostFieldLogin]
	if token == "" || login == "" {
		// A half-written blob must never mint a broken credential.
		return "", "", ErrNoLink
	}
	return login, token, nil
}
