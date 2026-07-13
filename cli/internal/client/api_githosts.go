package client

import "context"

// GitHost is one row of GET /api/git/hosts: an operator-allowlisted additional
// git host (Gitea/Forgejo) and whether this user has a PAT saved for it. The
// token itself is write-only and never returned.
type GitHost struct {
	Host   string `json:"host"`
	Linked bool   `json:"linked"`
	Login  string `json:"login,omitempty"`
}

type gitHostsResponse struct {
	Hosts []GitHost `json:"hosts"`
}

// ListGitHosts returns the allowlisted hosts with this user's link state.
func (c *Client) ListGitHosts(ctx context.Context) ([]GitHost, error) {
	var r gitHostsResponse
	err := c.Do(ctx, "GET", "/api/git/hosts", nil, &r)
	return r.Hosts, err
}

type setGitHostTokenRequest struct {
	Token string `json:"token"`
}

// SetGitHostToken validates and stores a PAT for host, returning the linked
// view (with the login the host reported). 400 bad_token when the host
// rejects it; 404 unknown_host when the host is not allowlisted.
func (c *Client) SetGitHostToken(ctx context.Context, host, token string) (GitHost, error) {
	var r GitHost
	err := c.Do(ctx, "PUT", "/api/git/hosts/"+host+"/token", setGitHostTokenRequest{Token: token}, &r)
	return r, err
}

// DeleteGitHostToken removes the stored PAT for host. Idempotent.
func (c *Client) DeleteGitHostToken(ctx context.Context, host string) error {
	return c.Do(ctx, "DELETE", "/api/git/hosts/"+host+"/token", nil, nil)
}
