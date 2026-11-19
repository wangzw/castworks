// Package client is the HTTP client the castworks command line uses to talk
// to the humanchannel control plane API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wangzw/castworks/pkg/humanchannel/api"
)

// defaultTimeout bounds a whole client call. It only guards against a
// connection hanging; the server answers once the database transaction
// commits and does not wait for the sandbox provider.
const defaultTimeout = 5 * time.Minute

// maxResponseBytes caps how much of a response body the client reads, so a
// misbehaving server cannot exhaust memory.
const maxResponseBytes = 1 << 20

// Client talks to the humanchannel HTTP API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client for the API at baseURL. It accepts only absolute http
// and https URLs with a host. When hc is nil, a client with a 5 minute
// timeout is used; otherwise New copies hc, so the caller's client is not
// modified.
func New(baseURL string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", baseURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q: must be an absolute http or https URL", baseURL)
	}
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	} else {
		clone := *hc
		hc = &clone
	}
	// A redirect is never followed: a 3xx response is surfaced as *APIError
	// like any other non-2xx status. CheckRedirect is set on the copy, never
	// on the client the caller passed in.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: hc}, nil
}

// Create creates a run instance and returns its ID.
func (c *Client) Create(ctx context.Context, prompt string) (runID string, err error) {
	var resp api.CreateRunResponse
	if err := c.do(ctx, http.MethodPost, "/v1/runs", api.CreateRunRequest{Prompt: prompt}, &resp); err != nil {
		return "", err
	}
	return resp.RunID, nil
}

// List returns all run instances, newest first.
func (c *Client) List(ctx context.Context) ([]api.RunSummary, error) {
	var runs []api.RunSummary
	if err := c.do(ctx, http.MethodGet, "/v1/runs", nil, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// Get returns the detail of one run instance.
func (c *Client) Get(ctx context.Context, runID string) (api.RunDetail, error) {
	var detail api.RunDetail
	if err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID), nil, &detail); err != nil {
		return api.RunDetail{}, err
	}
	return detail, nil
}

// Send appends a message to the inbox of a run instance and returns its
// sequence number.
func (c *Client) Send(ctx context.Context, runID string, text string) (inboxSeq int64, err error) {
	var resp api.SendMessageResponse
	if err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/messages", api.SendMessageRequest{Text: text}, &resp); err != nil {
		return 0, err
	}
	return resp.InboxSeq, nil
}

// End ends a run instance. It is idempotent.
func (c *Client) End(ctx context.Context, runID string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/end", nil, nil)
}

// do sends one request and decodes its response. Transport errors are
// returned as is; non-2xx responses become *APIError.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	// Every POST declares JSON, even one without a body such as /end: the
	// server rejects a POST that does not.
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp)
	}
	data, err := readResponseBody(resp.Body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// readResponseBody reads a response body, rejecting one larger than
// maxResponseBytes instead of buffering it.
func readResponseBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBytes)
	}
	return data, nil
}

// newAPIError builds an *APIError from a non-2xx response. The message is the
// response body's error field; when the body is not JSON or carries no error
// text, the HTTP status text is used.
func newAPIError(resp *http.Response) error {
	message := ""
	// The error body is small by contract; still cap the read.
	if data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes)); err == nil {
		var er api.ErrorResponse
		if json.Unmarshal(data, &er) == nil {
			message = er.Error
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Message: message}
}

// APIError is returned when the server responds with a non-2xx status.
type APIError struct {
	Status  int
	Message string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Message)
}
