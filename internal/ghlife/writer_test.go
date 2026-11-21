package ghlife

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/worker"
)

type tokenErr struct{ err error }

func (t tokenErr) Token(context.Context, string, map[string]string) (string, error) { return "", t.err }

func TestWriterClassifiesAnswers(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		header    string
		write     bool
		ambiguous bool
		permanent bool
	}{
		{"read 502", http.StatusBadGateway, "", false, false, false},
		{"write 502", http.StatusBadGateway, "", true, true, false},
		{"write 422", http.StatusUnprocessableEntity, "", true, false, true},
		{"write 429", http.StatusTooManyRequests, "", true, false, false},
		{"rate limited 403", http.StatusForbidden, "0", true, false, false},
		{"refused 403", http.StatusForbidden, "", true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.header != "" {
					w.Header().Set("X-RateLimit-Remaining", c.header)
				}
				w.WriteHeader(c.status)
				fmt.Fprint(w, `{"message":"nope"}`)
			}))
			defer srv.Close()
			w := &Writer{App: newFakeTokens(), BaseURL: srv.URL, HTTP: srv.Client()}
			method := http.MethodGet
			if c.write {
				method = http.MethodPost
			}
			err := w.REST(context.Background(), Repo{owner, repoNm}, githubapp.Checks, method, "/x", nil, nil)
			var st *ErrStatus
			if !errors.As(err, &st) || st.Status != c.status || st.Message != "nope" {
				t.Fatalf("error %v, want HTTP %d with GitHub's message", err, c.status)
			}
			if got := worker.IsAmbiguous(err); got != c.ambiguous {
				t.Errorf("ambiguous = %v, want %v (%v)", got, c.ambiguous, err)
			}
			if worker.IsPermanent(err) != c.permanent {
				t.Errorf("permanent = %v, want %v", !c.permanent, c.permanent)
			}
		})
	}
}

// newFakeTokens mints an empty token for every scope.
func newFakeTokens() TokenSource { return tokenErr{} }

func TestWriterTokenRefusalIsPermanent(t *testing.T) {
	w := &Writer{App: tokenErr{fmt.Errorf("%w: HTTP 422", githubapp.ErrRefused)}, BaseURL: "http://unused"}
	if err := w.REST(context.Background(), Repo{owner, repoNm}, githubapp.Checks, http.MethodGet, "/x", nil, nil); !worker.IsPermanent(err) {
		t.Errorf("refused token: %v, want permanent", err)
	}
	w.App = tokenErr{errors.New("github unreachable")}
	if err := w.REST(context.Background(), Repo{owner, repoNm}, githubapp.Checks, http.MethodGet, "/x", nil, nil); err == nil ||
		worker.IsPermanent(err) {
		t.Errorf("unreachable token service: %v, want a retried error", err)
	}
}

func TestGraphQLErrorsAreRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"message":"Could not resolve"},{"message":"twice"}]}`)
	}))
	defer srv.Close()
	w := &Writer{App: newFakeTokens(), BaseURL: srv.URL, HTTP: srv.Client()}
	err := w.GraphQL(context.Background(), Repo{owner, repoNm}, githubapp.PullRequests, "query { x }", nil, nil, true)
	if !errors.Is(err, ErrGraphQL) || err.Error() != "github graphql refused: Could not resolve; twice" {
		t.Errorf("graphql errors: %v", err)
	}
}
