package client

import "context"

// Repo is one GitHub repository the user has granted ProteOS access to, as
// returned by GET /api/git/repos — the set available to clone.
type Repo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
}

// ReposResult is the GET /api/git/repos envelope. GrantsURL links to the GitHub
// App's installation settings so the user can choose which repos ProteOS may see.
type ReposResult struct {
	Repos     []Repo `json:"repos"`
	GrantsURL string `json:"grants_url,omitempty"`
}

// ListRepos returns the repositories the user can clone, plus the grants URL.
func (c *Client) ListRepos(ctx context.Context) (ReposResult, error) {
	var r ReposResult
	err := c.Do(ctx, "GET", "/api/git/repos", nil, &r)
	return r, err
}
