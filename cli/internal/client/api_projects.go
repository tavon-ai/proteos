package client

import "context"

// Project is one cloned repository under /workspace on a machine, as returned by
// GET /api/projects. It mirrors the control-plane/guest Project shape.
type Project struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Remote        string `json:"remote,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Dirty         bool   `json:"dirty"`
	LastCommitAt  string `json:"last_commit_at,omitempty"`
	LastCommitMsg string `json:"last_commit_msg,omitempty"`
}

type projectsResponse struct {
	Projects []Project `json:"projects"`
}

// ListProjects returns the repositories cloned in a running machine's workspace.
// The machine must be running (the list is fetched live over the control channel).
func (c *Client) ListProjects(ctx context.Context, machineID string) ([]Project, error) {
	var r projectsResponse
	err := c.Do(ctx, "GET", "/api/projects?machine="+machineID, nil, &r)
	return r.Projects, err
}

type cloneRequest struct {
	FullName string `json:"full_name"`
}

type cloneResponse struct {
	OpID string `json:"op_id"`
}

// Clone dispatches an asynchronous clone of owner/repo into a machine's
// workspace. It returns an op id immediately; the clone completes in the
// background (observe by polling ListProjects until the repo appears).
func (c *Client) Clone(ctx context.Context, machineID, fullName string) (string, error) {
	var r cloneResponse
	err := c.Do(ctx, "POST", "/api/git/clone?machine="+machineID, cloneRequest{FullName: fullName}, &r)
	return r.OpID, err
}
