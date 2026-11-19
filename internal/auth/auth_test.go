package auth

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/event"
)

// hmacKBody is HMAC-SHA256("k", "body"), computed with Python's hmac.
const hmacKBody = "c6d811ef3aeb02437cd423f1abe13209041864630bdc4e2c04def5c7b0031a23"

var keys = Keys{
	event.SourceVikunja: []byte("k"), event.SourceGitHub: []byte("k"),
	event.SourceWikiComment: []byte("k"), event.SourceImageBuild: []byte("k"),
}

func delivery(source event.Source, header, value string) event.Delivery {
	return event.Delivery{Source: source, Body: []byte("body"), Headers: map[string]string{header: value}}
}

func TestVerifyAcceptsEachSourceFormat(t *testing.T) {
	cases := []event.Delivery{
		delivery(event.SourceVikunja, VikunjaHeader, hmacKBody),
		delivery(event.SourceGitHub, GitHubHeader, "sha256="+hmacKBody),
		delivery(event.SourceWikiComment, DispatchHeader, "sha256="+hmacKBody),
		delivery(event.SourceImageBuild, DispatchHeader, "sha256="+hmacKBody),
	}
	for _, d := range cases {
		if err := keys.Verify(d); err != nil {
			t.Errorf("%s: %v", d.Source, err)
		}
	}
}

func TestVerifyRefusals(t *testing.T) {
	big := delivery(event.SourceVikunja, VikunjaHeader, hmacKBody)
	big.Body = []byte(strings.Repeat("x", MaxVikunjaBodyBytes+1))
	cases := map[string]struct {
		keys Keys
		d    event.Delivery
		want error
	}{
		"unsigned":                         {keys, delivery(event.SourceGitHub, "X-Other", "x"), ErrUnsigned},
		"wrong digest":                     {keys, delivery(event.SourceGitHub, GitHubHeader, "sha256="+strings.Repeat("0", 64)), ErrBadSignature},
		"github no prefix":                 {keys, delivery(event.SourceGitHub, GitHubHeader, hmacKBody), ErrBadSignature},
		"vikunja prefixed":                 {keys, delivery(event.SourceVikunja, VikunjaHeader, "sha256="+hmacKBody), ErrBadSignature},
		"other source key":                 {Keys{event.SourceGitHub: []byte("k")}, delivery(event.SourceVikunja, VikunjaHeader, hmacKBody), ErrBadSignature},
		"github header on dispatch source": {keys, delivery(event.SourceWikiComment, GitHubHeader, "sha256="+hmacKBody), ErrUnsigned},
		"unknown source":                   {keys, delivery("cluster-triage", DispatchHeader, "sha256="+hmacKBody), ErrUnknownSource},
		"too large":                        {keys, big, ErrTooLarge},
	}
	for name, c := range cases {
		if err := c.keys.Verify(c.d); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
}

func TestStatus(t *testing.T) {
	cases := map[error]int{
		nil: 0, ErrUnsigned: http.StatusUnauthorized, ErrBadSignature: http.StatusForbidden,
		ErrUnknownSource: http.StatusNotFound, ErrTooLarge: http.StatusRequestEntityTooLarge,
		event.ErrMalformed: http.StatusBadRequest,
	}
	for err, want := range cases {
		if got := Status(err); got != want {
			t.Errorf("Status(%v) = %d, want %d", err, got, want)
		}
	}
}

func TestSign(t *testing.T) {
	header, value, err := Sign(event.SourceGitHub, []byte("k"), []byte("body"))
	if err != nil || header != GitHubHeader || value != "sha256="+hmacKBody {
		t.Errorf("Sign = %q %q %v", header, value, err)
	}
	if _, _, err := Sign("nope", nil, nil); !errors.Is(err, ErrUnknownSource) {
		t.Errorf("unknown source: %v", err)
	}
}
