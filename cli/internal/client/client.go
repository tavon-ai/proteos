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

	// MaxAttempts bounds how many times Do tries a request, including the first
	// (so 1 disables retries). Zero means DefaultMaxAttempts.
	MaxAttempts int
	// RetryBaseDelay / RetryMaxDelay bound the exponential backoff between
	// retries. Zero means the Default* constants.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// Retry defaults for Do: three attempts (one original + two retries), starting
// at 200ms and doubling up to 2s. Deliberately gentler than the SSE stream's
// backoff (sse.go) since a foreground CLI command has a human waiting on it.
const (
	DefaultMaxAttempts    = 3
	DefaultRetryBaseDelay = 200 * time.Millisecond
	DefaultRetryMaxDelay  = 2 * time.Second
)

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
//
// Transient failures are retried with exponential backoff (see MaxAttempts /
// RetryBaseDelay / RetryMaxDelay), but only for GET: a dropped connection can
// happen after the server already received and processed a POST/PUT/DELETE, so
// re-sending one risks a duplicate effect (there is no idempotency-key
// machinery here to make that safe). GET has no such risk either way — a lost
// response or a 429/5xx just means try the read again.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		bodyBytes = b
	}

	attempts := 1
	if method == http.MethodGet {
		attempts = c.maxAttempts()
	}
	delay := c.retryBaseDelay()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
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
			lastErr = fmt.Errorf("request %s %s: %w", method, path, err)
			if attempt == attempts {
				return lastErr
			}
			if !sleepBackoff(ctx, &delay, c.retryMaxDelay()) {
				return ctx.Err()
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := decodeAPIError(resp)
			resp.Body.Close()
			if retryableStatus(apiErr.Status) && attempt < attempts {
				lastErr = apiErr
				if !sleepBackoff(ctx, &delay, c.retryMaxDelay()) {
					return ctx.Err()
				}
				continue
			}
			return apiErr
		}

		defer resp.Body.Close()
		if out == nil {
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	return lastErr
}

// maxAttempts / retryBaseDelay / retryMaxDelay apply the Default* constants
// when the client's fields are unset (the New() zero-value case).
func (c *Client) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (c *Client) retryBaseDelay() time.Duration {
	if c.RetryBaseDelay > 0 {
		return c.RetryBaseDelay
	}
	return DefaultRetryBaseDelay
}

func (c *Client) retryMaxDelay() time.Duration {
	if c.RetryMaxDelay > 0 {
		return c.RetryMaxDelay
	}
	return DefaultRetryMaxDelay
}

// retryableStatus reports whether status is a transient server condition worth
// retrying: 429 (rate limited) or any 5xx.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// sleepBackoff waits *delay (or until ctx is done, whichever comes first) and
// doubles *delay up to max. Returns false if ctx ended the wait early.
func sleepBackoff(ctx context.Context, delay *time.Duration, max time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(*delay):
	}
	if *delay *= 2; *delay > max {
		*delay = max
	}
	return true
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
