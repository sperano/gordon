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
	"os"
	"strings"
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
	// Path is the request's path relative to BaseURL, without its query;
	// "" for a failure before any request was built. It names the route a
	// token was refused on, so the missing permission can be told apart.
	Path string
	Err  error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Provider, e.Class)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Path != "" {
		msg += " on GET " + e.Path
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
	// Authorization is the full header value ("Bearer <token>"); "" sends
	// none. Set this directly only for a token that never rotates
	// (tests); a production client sets TokenFile instead, which takes
	// precedence.
	Authorization string
	// TokenFile, when set, is read fresh for every request (a rotated
	// Secret applies without a restart) and sent as "Bearer <contents>".
	TokenFile string
	Headers   map[string]string
	HTTP      *http.Client
	// Limit bounds the requests in flight to this provider; nil is unbounded.
	Limit   chan struct{}
	Observe Observer
}

// authorization is the request's Authorization header value: TokenFile's
// current contents when set, else the static Authorization.
func (c *Client) authorization() (string, error) {
	if c.TokenFile == "" {
		return c.Authorization, nil
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("token: %s is empty", c.TokenFile)
	}
	return "Bearer " + token, nil
}

// NewLimit returns a limit of n requests in flight; nil when n <= 0.
func NewLimit(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// get reads path (relative to BaseURL, already escaped) with query into
// out. valid, when set, checks the decoded response (that it is the object
// asked for); its error is an invalid read, observed as such.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any, valid func() error) error {
	return c.getCapped(ctx, path, query, out, valid, MaxResponseBytes)
}

// getCapped is get with the body capped at limit bytes (at most
// MaxResponseBytes): a longer body is Invalid, read no further than the cap.
func (c *Client) getCapped(ctx context.Context, path string, query url.Values, out any, valid func() error, limit int) error {
	limit = min(limit, MaxResponseBytes)
	if c.Limit != nil {
		select {
		case c.Limit <- struct{}{}:
			defer func() { <-c.Limit }()
		case <-ctx.Done():
			return c.fail(path, Unavailable, 0, ctx.Err(), 0)
		}
	}
	start := time.Now()
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return c.fail(path, Invalid, 0, err, time.Since(start))
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	auth, err := c.authorization()
	if err != nil {
		return c.fail(path, Unavailable, 0, err, time.Since(start))
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The error quotes the request URL, which holds no credential
		// (the token travels in a header).
		return c.fail(path, Unavailable, 0, err, time.Since(start))
	}
	defer resp.Body.Close()
	if class, failed := classify(resp); failed {
		io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes)) //nolint:errcheck // drained for reuse
		return c.fail(path, class, resp.StatusCode, nil, time.Since(start))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return c.fail(path, Unavailable, resp.StatusCode, err, time.Since(start))
	}
	if len(body) > limit {
		return c.fail(path, Invalid, resp.StatusCode, fmt.Errorf("response over %d bytes", limit), time.Since(start))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return c.fail(path, Invalid, resp.StatusCode, err, time.Since(start))
	}
	if raw, ok := out.(*json.RawMessage); ok {
		// json.Unmarshal strips surrounding whitespace from RawMessage;
		// Kelos quotes the HTTP body byte-for-byte in task prompts.
		*raw = body
	}
	if valid != nil {
		if err := valid(); err != nil {
			return c.fail(path, Invalid, resp.StatusCode, err, time.Since(start))
		}
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

func (c *Client) fail(path string, class Class, status int, err error, d time.Duration) error {
	c.observe(string(class), d)
	return &Error{Provider: c.Provider, Class: class, Status: status, Path: path, Err: err}
}

func (c *Client) observe(result string, d time.Duration) {
	if c.Observe != nil {
		c.Observe(c.Provider, result, d)
	}
}
