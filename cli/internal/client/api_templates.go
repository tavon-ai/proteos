package client

import "context"

// Template is a machine template (the "type" of a machine: full-stack, go, …) as
// returned by GET /api/templates. Only the fields the CLI renders are kept.
type Template struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// ListTemplates returns the machine-template catalog. A legacy single-image
// deployment returns an empty array.
func (c *Client) ListTemplates(ctx context.Context) ([]Template, error) {
	var ts []Template
	err := c.Do(ctx, "GET", "/api/templates", nil, &ts)
	return ts, err
}
