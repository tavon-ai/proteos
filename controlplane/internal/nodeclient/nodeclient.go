// Package nodeclient is the control plane's HTTP client for the node-agent's
// agentapi wire contract. It is the only place the control plane talks to an
// agent; the direction of trust is one-way (control plane dials agent), so this
// holds the single shared bearer credential. The agentapi types come from the
// nodeagent module via the workspace (go.work) — no replace directive.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentapi "github.com/tavon/proteos/nodeagent/api"
)

// ErrUnknownMachine is returned when the agent reports 404 unknown_machine.
var ErrUnknownMachine = errors.New("nodeclient: agent does not know this machine")

// Client dials a single node-agent.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the agent at baseURL authenticating with token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Ensure issues PUT /v1/machines/{id} (idempotent ensure-running).
func (c *Client) Ensure(ctx context.Context, id string, req agentapi.EnsureRequest) (agentapi.EnsureResponse, error) {
	var out agentapi.EnsureResponse
	err := c.do(ctx, http.MethodPut, "/v1/machines/"+id, req, &out, http.StatusAccepted)
	return out, err
}

// Stop issues POST /v1/machines/{id}/stop (graceful shutdown).
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/machines/"+id+"/stop", nil, nil, http.StatusAccepted)
}

// Status issues GET /v1/machines/{id}. A 404 maps to ErrUnknownMachine.
func (c *Client) Status(ctx context.Context, id string) (agentapi.MachineStatus, error) {
	var out agentapi.MachineStatus
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+id, nil, &out, http.StatusOK)
	return out, err
}

// List issues GET /v1/machines (reconciliation).
func (c *Client) List(ctx context.Context) ([]agentapi.MachineStatus, error) {
	var out agentapi.ListResponse
	err := c.do(ctx, http.MethodGet, "/v1/machines", nil, &out, http.StatusOK)
	return out.Machines, err
}

// Destroy issues DELETE /v1/machines/{id} (cleanup).
func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/machines/"+id, nil, nil, http.StatusNoContent)
}

// Health issues GET /healthz (unauthenticated on the agent, but we send the
// header anyway — it is ignored).
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil, http.StatusOK)
}

// do performs one request, encoding body (if non-nil) and decoding into out (if
// non-nil), asserting the response status equals wantStatus. A 404 is mapped to
// ErrUnknownMachine before the status check so callers can branch on it.
func (c *Client) do(ctx context.Context, method, path string, body, out any, wantStatus int) error {
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set(agentapi.AuthHeader, agentapi.BearerPrefix+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrUnknownMachine
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: agent returned %d (want %d)", method, path, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
