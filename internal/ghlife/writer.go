// Package ghlife performs Gordon's GitHub writes (Gordon 08): the outbox
// executors of the 👀 reaction, the follow-up draft label and
// conversion, the fix-suggestions label removal, skipped review checks
// and the pull-request notices (start, round caps), as the GitHub App.
// Every write is preceded by a read of the state it changes, so a
// repeated or recovered action that already applied writes nothing
// twice; a notice is found again by its marker on a comment by a
// pull-request author (the App's bot or a Workspace token's user).
package ghlife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/upstream"
	"github.com/sperano/gordon/internal/worker"
)

// maxResponseBytes caps one GitHub answer.
const maxResponseBytes = 8 << 20

// TokenSource mints the App's installation tokens.
type TokenSource interface {
	Token(ctx context.Context, repo string, perms map[string]string) (string, error)
}

// Writer is a GitHub API client acting as the App, one narrowed token
// per repository and kind of write.
type Writer struct {
	App TokenSource
	// BaseURL is the API base (https://api.github.com).
	BaseURL string
	HTTP    *http.Client
}

// ErrStatus is a GitHub answer outside 2xx.
type ErrStatus struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *ErrStatus) Error() string {
	return fmt.Sprintf("github %s %s: HTTP %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports a GitHub 404.
func IsNotFound(err error) bool {
	var st *ErrStatus
	return errors.As(err, &st) && st.Status == http.StatusNotFound
}

// Repo is the repository a target names.
type Repo struct{ Owner, Name string }

// FullName is owner/name.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Target is a parsed GitHub action target: github/<owner>/<name>/<kind>/<n>,
// kind "pull" for a pull request, c, l or r for a comment or review.
type Target struct {
	Repo
	Kind string
	N    int64
}

// ErrNotGitHub is an action this executor does not write.
var ErrNotGitHub = errors.New("not a GitHub action")

// ParseTarget parses a GitHub action target.
func ParseTarget(a action.Action) (Target, error) {
	parts := strings.Split(string(a.Target), "/")
	if len(parts) != 5 || parts[0] != upstream.ProviderGitHub {
		return Target{}, worker.Permanent(fmt.Errorf("%w: %s", ErrNotGitHub, a.Target))
	}
	n, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil || n <= 0 || !upstream.SafeRepository(parts[1]+"/"+parts[2]) {
		return Target{}, worker.Permanent(fmt.Errorf("%w: %s", ErrNotGitHub, a.Target))
	}
	return Target{Repo: Repo{parts[1], parts[2]}, Kind: parts[3], N: n}, nil
}

// token mints the scope's token; a refusal cannot be retried away.
func (w *Writer) token(ctx context.Context, r Repo, perms map[string]string) (string, error) {
	t, err := w.App.Token(ctx, r.Name, perms)
	if errors.Is(err, githubapp.ErrRefused) {
		return "", worker.Permanent(err)
	}
	return t, err
}

// do sends one request with token and decodes the answer into out (nil:
// discarded). A read failure is a plain error (retried). A write that
// failed at the transport after it was sent, or that GitHub answered
// with a 5xx, may have applied: Ambiguous. A 4xx other than 408, 429 and
// a rate limit is Permanent.
func (w *Writer) do(ctx context.Context, token, method, path string, body, out any, write bool) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return worker.Permanent(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.BaseURL+path, rd)
	if err != nil {
		return worker.Permanent(err)
	}
	for k, v := range upstream.GitHubHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := w.HTTP.Do(req)
	if err != nil {
		if write {
			return worker.Ambiguous(err)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if write {
			return worker.Ambiguous(err)
		}
		return err
	}
	return classify(method, path, resp, write, data, out)
}

func classify(method, path string, resp *http.Response, write bool, data []byte, out any) error {
	status := resp.StatusCode
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &msg) //nolint:errcheck // a message is optional
	st := &ErrStatus{Method: method, Path: path, Status: status, Message: msg.Message}
	limited := status == http.StatusForbidden && rateLimited(resp)
	switch {
	case status >= http.StatusInternalServerError && write:
		return worker.Ambiguous(st)
	case status >= http.StatusInternalServerError, status == http.StatusTooManyRequests,
		status == http.StatusRequestTimeout, limited:
		return st
	case status >= http.StatusBadRequest:
		return worker.Permanent(st)
	case out == nil:
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	return nil
}

// rateLimited reports a 403 that is a rate limit, retried: the primary
// limit spent (X-RateLimit-Remaining: 0) or a secondary limit, which
// answers with Retry-After. GitHub refuses a limited request before
// acting on it, so a limited write is no ambiguous one.
func rateLimited(resp *http.Response) bool {
	return resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""
}

// REST calls path (relative to BaseURL, already escaped).
func (w *Writer) REST(ctx context.Context, r Repo, perms map[string]string, method, path string, body, out any) error {
	token, err := w.token(ctx, r, perms)
	if err != nil {
		return err
	}
	return w.do(ctx, token, method, path, body, out, method != http.MethodGet)
}

// ErrGraphQL is a GraphQL answer carrying errors: GitHub reports a
// refused query or mutation in a 200 answer, and applies no part of it.
var ErrGraphQL = errors.New("github graphql refused")

// GraphQL sends a query (write false) or a mutation (write true) and
// decodes its data into out.
func (w *Writer) GraphQL(ctx context.Context, r Repo, perms map[string]string, query string, vars map[string]any,
	out any, write bool) error {
	token, err := w.token(ctx, r, perms)
	if err != nil {
		return err
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": vars}
	if err := w.do(ctx, token, http.MethodPost, "/graphql", body, &answer, write); err != nil {
		return err
	}
	if len(answer.Errors) > 0 {
		msgs := make([]string, len(answer.Errors))
		for i, e := range answer.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("%w: %s", ErrGraphQL, strings.Join(msgs, "; "))
	}
	if out == nil || len(answer.Data) == 0 {
		return nil
	}
	return json.Unmarshal(answer.Data, out)
}

// path escapes each segment of a REST path.
func path(segments ...string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteString("/" + url.PathEscape(s))
	}
	return b.String()
}
