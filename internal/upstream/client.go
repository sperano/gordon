// Package upstream reads the fresh state Gordon decides on: Vikunja tasks
// and GitHub pull requests. It only reads (every request is a GET), so a
// worker holding these clients cannot change GitHub or Vikunja; writes
// belong to the outbox executors.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MaxResponseBytes caps one response body.
const MaxResponseBytes = 8 << 20

// Providers.
const (
	ProviderVikunja = "vikunja"
	ProviderGitHub  = "github"
)

// Class is why a read failed.
type Class string

const (
	NotFound     Class = "not_found"
	Unauthorized Class = "unauthorized"
	RateLimited  Class = "rate_limited"
	Unavailable  Class = "unavailable"
	Invalid      Class = "invalid"
)

// Result labels the observer gets: a Class, or ResultOK.
const ResultOK = "ok"

// Error is a failed read.
type Error struct {
	Provider string
	Class    Class
	Status   int
	Err      error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Provider, e.Class)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf is err's Class, Unavailable for an error from elsewhere.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return Unavailable
}

// Observer records one request: its provider, result and duration.
type Observer func(provider, result string, d time.Duration)

// Client is a bounded, authenticated JSON reader for one provider.
type Client struct {
	Provider string
	BaseURL  string
	// Authorization is the full header value ("Bearer <token>"); "" sends none.
	Authorization string
	Headers       map[string]string
	HTTP          *http.Client
	// Limit bounds the requests in flight to this provider; nil is unbounded.
	Limit   chan struct{}
	Observe Observer
}

// NewLimit returns a limit of n requests in flight; nil when n <= 0.
func NewLimit(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// get reads path (relative to BaseURL, already escaped) with query into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if c.Limit != nil {
		select {
		case c.Limit <- struct{}{}:
			defer func() { <-c.Limit }()
		case <-ctx.Done():
			return c.fail(Unavailable, 0, ctx.Err(), 0)
		}
	}
	start := time.Now()
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return c.fail(Invalid, 0, err, time.Since(start))
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	if c.Authorization != "" {
		req.Header.Set("Authorization", c.Authorization)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL holds no credential; the error names the host only.
		return c.fail(Unavailable, 0, err, time.Since(start))
	}
	defer resp.Body.Close()
	if class, failed := classify(resp); failed {
		io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes)) //nolint:errcheck // drained for reuse
		return c.fail(class, resp.StatusCode, nil, time.Since(start))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return c.fail(Unavailable, resp.StatusCode, err, time.Since(start))
	}
	if len(body) > MaxResponseBytes {
		return c.fail(Invalid, resp.StatusCode, fmt.Errorf("response over %d bytes", MaxResponseBytes), time.Since(start))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return c.fail(Invalid, resp.StatusCode, err, time.Since(start))
	}
	c.observe(ResultOK, time.Since(start))
	return nil
}

// classify maps a status to a failure class; failed is false for 2xx.
func classify(resp *http.Response) (Class, bool) {
	switch s := resp.StatusCode; {
	case s >= http.StatusOK && s < http.StatusMultipleChoices:
		return "", false
	case s == http.StatusNotFound || s == http.StatusGone:
		return NotFound, true
	case s == http.StatusTooManyRequests || (s == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0"):
		return RateLimited, true
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return Unauthorized, true
	case s >= http.StatusInternalServerError:
		return Unavailable, true
	default:
		return Invalid, true
	}
}

func (c *Client) fail(class Class, status int, err error, d time.Duration) error {
	c.observe(string(class), d)
	return &Error{Provider: c.Provider, Class: class, Status: status, Err: err}
}

func (c *Client) observe(result string, d time.Duration) {
	if c.Observe != nil {
		c.Observe(c.Provider, result, d)
	}
}
