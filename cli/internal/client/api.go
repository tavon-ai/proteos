package client

import "context"

// User is the authenticated identity from GET /api/me.
type User struct {
	Login     string `json:"login"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// Machine is a subset of the control-plane MachineSummary the CLI renders.
type Machine struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	State      string  `json:"state"`
	GuestIP    *string `json:"guest_ip"`
	TemplateID *string `json:"template_id"`
	CreatedAt  string  `json:"created_at"`
}

// Me is the GET /api/me response.
type Me struct {
	User     User      `json:"user"`
	Machines []Machine `json:"machines"`
}

// Me returns the authenticated user and their machines. A 401 here is how the
// CLI verifies a token at `auth login`.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var m Me
	err := c.Do(ctx, "GET", "/api/me", nil, &m)
	return m, err
}

// ListMachines returns the caller's machines (GET /api/machines returns a bare
// array).
func (c *Client) ListMachines(ctx context.Context) ([]Machine, error) {
	var ms []Machine
	err := c.Do(ctx, "GET", "/api/machines", nil, &ms)
	return ms, err
}

// GetMachine returns one machine by id.
func (c *Client) GetMachine(ctx context.Context, id string) (Machine, error) {
	var m Machine
	err := c.Do(ctx, "GET", "/api/machines/"+id, nil, &m)
	return m, err
}

// CreateMachineRequest is the POST /api/machines body. A zero-value resource
// pointer (nil) falls back to the template's default server-side.
type CreateMachineRequest struct {
	Name       string `json:"name,omitempty"`
	TemplateID string `json:"template_id,omitempty"`
	Vcpus      *int   `json:"vcpus,omitempty"`
	MemMiB     *int   `json:"mem_mib,omitempty"`
	DiskMiB    *int   `json:"disk_mib,omitempty"`
}

// CreateMachine provisions a new machine and returns its summary. The server
// replies 202: the machine boots asynchronously, so State is typically still a
// provisioning state on return.
func (c *Client) CreateMachine(ctx context.Context, req CreateMachineRequest) (Machine, error) {
	var m Machine
	err := c.Do(ctx, "POST", "/api/machines", req, &m)
	return m, err
}

// StartMachine resumes a stopped machine and returns the updated summary.
func (c *Client) StartMachine(ctx context.Context, id string) (Machine, error) {
	var m Machine
	err := c.Do(ctx, "POST", "/api/machines/"+id+"/start", struct{}{}, &m)
	return m, err
}

// StopMachine stops (hibernates) a running machine and returns the updated
// summary.
func (c *Client) StopMachine(ctx context.Context, id string) (Machine, error) {
	var m Machine
	err := c.Do(ctx, "POST", "/api/machines/"+id+"/stop", struct{}{}, &m)
	return m, err
}
