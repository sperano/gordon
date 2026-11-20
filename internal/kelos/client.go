package kelos

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// In-cluster defaults: the API server's Service and the pod's projected
// ServiceAccount token and CA.
const (
	InClusterURL       = "https://kubernetes.default.svc"
	InClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	InClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// Response caps: a Task (its prompt holds the task comments) and one list
// page.
const (
	maxObjectBytes = 4 << 20
	maxListBytes   = 32 << 20
	// listPageSize bounds one list page; continue tokens fetch the rest.
	listPageSize = 250
	// minListPageSize is the largest page sure to fit maxListBytes: a
	// page over the cap is asked again at half the size, down to it.
	minListPageSize = maxListBytes / maxObjectBytes
	// maxListPages stops a list that never ends (a misbehaving server).
	maxListPages = 400
	// maxMessageBytes caps the API server message an error keeps.
	maxMessageBytes = 512
)

// Kubernetes Status reasons Gordon tells apart.
const (
	ReasonAlreadyExists = "AlreadyExists"
	ReasonNotFound      = "NotFound"
)

// Observer records one API request: its operation, result and duration.
type Observer func(op, result string, d time.Duration)

// Client reads and creates Tasks in one namespace.
type Client struct {
	BaseURL   string
	Namespace string
	// TokenFile is re-read for every request: projected ServiceAccount
	// tokens rotate. "" sends no Authorization header (tests).
	TokenFile string
	HTTP      *http.Client
	Observe   Observer
}

// NewHTTPClient returns an HTTP client trusting caFile ("" keeps the
// system roots) with the given per-request timeout.
func NewHTTPClient(caFile string, timeout time.Duration) (*http.Client, error) {
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("http.DefaultTransport is a %T, not an *http.Transport", http.DefaultTransport)
	}
	tr := def.Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificate", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// Error is a failed API request.
type Error struct {
	Op     string
	Status int
	// Reason is the Kubernetes Status reason (AlreadyExists, Invalid, ...).
	Reason  string
	Message string
	// Sent is true when the request may have reached the API server
	// (anything past dialing): a create may then have been applied.
	Sent bool
	Err  error
}

func (e *Error) Error() string {
	msg := "kelos " + e.Op
	if e.Status != 0 {
		msg += fmt.Sprintf(": HTTP %d", e.Status)
	}
	if e.Reason != "" {
		msg += " " + e.Reason
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func asError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsAlreadyExists reports a create refused because the name is taken.
func IsAlreadyExists(err error) bool {
	e, ok := asError(err)
	return ok && e.Status == http.StatusConflict && e.Reason == ReasonAlreadyExists
}

// IsNotFound reports a Task that does not exist.
func IsNotFound(err error) bool {
	e, ok := asError(err)
	return ok && e.Status == http.StatusNotFound
}

// IsRefused reports a request the API server or admission refused as
// such (invalid, forbidden, denied by a policy): retrying cannot help.
func IsRefused(err error) bool {
	e, ok := asError(err)
	if !ok {
		return false
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

// IsAmbiguous reports a write whose outcome is unknown: the request may
// have been applied (a transport error after sending, a 5xx or a timeout,
// or a success answer that could not be read).
func IsAmbiguous(err error) bool {
	e, ok := asError(err)
	if !ok {
		return false
	}
	return e.Sent && (e.Status < http.StatusMultipleChoices || e.Status >= http.StatusInternalServerError)
}

func (c *Client) collection() string {
	return fmt.Sprintf("%s/apis/%s/%s/namespaces/%s/%s", c.BaseURL, Group, Version, url.PathEscape(c.Namespace), resource)
}

// Create creates t in the client's namespace and returns it as stored.
func (c *Client) Create(ctx context.Context, t Task) (Task, error) {
	t.APIVersion, t.Kind, t.Metadata.Namespace = APIVersion, KindTask, c.Namespace
	body, err := json.Marshal(t)
	if err != nil {
		return Task{}, &Error{Op: "create", Err: err}
	}
	var out Task
	err = c.do(ctx, "create", http.MethodPost, c.collection(), nil, body, maxObjectBytes, &out)
	return out, err
}

// Get reads the Task of that name, and its raw JSON.
func (c *Client) Get(ctx context.Context, name string) (Task, json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "get", http.MethodGet, c.collection()+"/"+url.PathEscape(name), nil, nil, maxObjectBytes, &raw); err != nil {
		return Task{}, nil, err
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return Task{}, nil, &Error{Op: "get", Err: err}
	}
	return t, raw, nil
}

type taskList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []Task `json:"items"`
}

// errTooLarge is a response over its byte cap.
var errTooLarge = errors.New("response too large")

// List returns every Task matching selector ("" for all), page by page.
// A page over maxListBytes (large Tasks) is read again in smaller pages.
func (c *Client) List(ctx context.Context, selector string) ([]Task, error) {
	var out []Task
	token, size := "", listPageSize
	for range maxListPages {
		q := url.Values{"limit": {fmt.Sprint(size)}}
		if selector != "" {
			q.Set("labelSelector", selector)
		}
		if token != "" {
			q.Set("continue", token)
		}
		var page taskList
		err := c.do(ctx, "list", http.MethodGet, c.collection(), q, nil, maxListBytes, &page)
		if errors.Is(err, errTooLarge) && size > minListPageSize {
			size = max(size/2, minListPageSize)
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if token = page.Metadata.Continue; token == "" {
			return out, nil
		}
	}
	return nil, &Error{Op: "list", Err: fmt.Errorf("more than %d pages", maxListPages)}
}

// status is a Kubernetes Status body.
type status struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, op, method, u string, q url.Values, body []byte, limit int64, out any) error {
	start := time.Now()
	err := c.send(ctx, op, method, u, q, body, limit, out)
	c.observe(op, err, time.Since(start))
	return err
}

func (c *Client) send(ctx context.Context, op, method, u string, q url.Values, body []byte, limit int64, out any) error {
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(req); err != nil {
		return &Error{Op: op, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return &Error{Op: op, Err: err} // ended before the request left
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Op: op, Sent: !dialError(err), Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Sent: true, Err: err}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var st status
		_ = json.Unmarshal(data, &st) //nolint:errcheck // a body that is no Status keeps the code alone
		return &Error{Op: op, Status: resp.StatusCode, Reason: st.Reason, Message: truncate(st.Message), Sent: true}
	}
	if int64(len(data)) > limit {
		return &Error{Op: op, Status: resp.StatusCode, Sent: true, Err: fmt.Errorf("%w: over %d bytes", errTooLarge, limit)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Sent: true, Err: err}
	}
	return nil
}

func (c *Client) authorize(req *http.Request) error {
	if c.TokenFile == "" {
		return nil
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return fmt.Errorf("token: %s is empty", c.TokenFile)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// dialError reports a failure to connect: the request never left.
func dialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// truncate caps s at maxMessageBytes, cut on a rune boundary so the
// message stays valid UTF-8.
func truncate(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	end := maxMessageBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// Results of an observed request.
const (
	ResultOK       = "ok"
	ResultNotFound = "not_found"
	ResultRefused  = "refused"
	ResultError    = "error"
)

// Results lists every result an observer may get, for bounded labels.
var Results = []string{ResultOK, ResultNotFound, ResultRefused, ResultError}

// Ops lists every observed operation.
var Ops = []string{"create", "get", "list"}

func (c *Client) observe(op string, err error, d time.Duration) {
	if c.Observe == nil {
		return
	}
	result := ResultOK
	switch {
	case err == nil:
	case IsNotFound(err):
		result = ResultNotFound
	case IsRefused(err) || IsAlreadyExists(err):
		result = ResultRefused
	default:
		result = ResultError
	}
	c.Observe(op, result, d)
}
