// Package client is a thin HTTP client for the ProteOS control-plane API. It
// authenticates with a personal access token (Authorization: Bearer), decodes
// the server's {error,detail} envelope into a typed error, and maps HTTP status
// to the CLI's documented exit codes.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Exit codes the CLI returns. Commands translate an *APIError via ExitCode();
// other failures use ExitError / ExitUsage directly.
const (
	ExitOK       = 0
	ExitError    = 1 // generic/runtime failure
	ExitUsage    = 2 // bad invocation
	ExitAuth     = 3 // 401/403
	ExitNotFound = 4 // 404
	ExitTaskFail = 5 // a task ended failed/canceled
)

// userAgent identifies the CLI in request logs. Version is stamped at build time.
var userAgent = "proteos-cli/dev"

// SetUserAgent overrides the User-Agent (called from main with the build version).
func SetUserAgent(ua string) { userAgent = ua }

// Client talks to one control-plane base URL with one bearer token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for baseURL authenticating with token. baseURL may carry
// a trailing slash; it is trimmed.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a non-2xx response decoded from the {error,detail} envelope.
type APIError struct {
	Status int
	Code   string
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%s)", e.Code, e.Detail)
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("http %d", e.Status)
}

// ExitCode maps the HTTP status to a CLI exit code.
func (e *APIError) ExitCode() int {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ExitAuth
	case http.StatusNotFound:
		return ExitNotFound
	default:
		return ExitError
	}
}

// NewRequest builds an authenticated request with the standard headers. Exposed
// so the SSE consumer can build a streaming GET.
func (c *Client) NewRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// Do issues an authenticated JSON request. A non-nil body is JSON-encoded; a
// non-nil out is JSON-decoded from a 2xx response. Non-2xx yields an *APIError.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := c.NewRequest(ctx, method, path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// decodeAPIError reads the error envelope from a non-2xx response.
func decodeAPIError(resp *http.Response) *APIError {
	e := &APIError{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var env struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(b, &env) == nil {
		e.Code = env.Error
		e.Detail = env.Detail
	}
	return e
}

// ExitCodeFor returns the appropriate process exit code for err: an *APIError's
// mapped code, ExitOK for nil, else ExitError.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	if ae, ok := errors.AsType[*APIError](err); ok {
		return ae.ExitCode()
	}
	return ExitError
}
