package auth

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/event"
)

// Each source has its own key; the expected signatures are HMAC-SHA256 of
// "body" under that key, computed with Python's hmac.
const (
	hmacKBody      = "c6d811ef3aeb02437cd423f1abe13209041864630bdc4e2c04def5c7b0031a23" // key "k"
	vikunjaSig     = "6a21795cb8c9f4307ac5863a97a6e3b881ea70ef49044028e160f9305dda41aa" // key "kv"
	githubSig      = "35ca91d60e991570647459012a8da80bd8d7c4e38049bcef4828cc9c05aa8d08" // key "kg"
	wikiCommentSig = "0bd56dde042823f5e551884765e5b398bda4d8ff30ce175dcbc79c2b2393817a" // key "kw"
	imageBuildSig  = "4a7b245131ac3811479ea35f20dc4634ce4e2b9b32cf5af61ecdb4a8313a73d6" // key "ki"
	githubKey      = "kg"
	imageBuildKey  = "ki"
)

var keys = Keys{
	event.SourceVikunja: []byte("kv"), event.SourceGitHub: []byte(githubKey),
	event.SourceWikiComment: []byte("kw"), event.SourceImageBuild: []byte(imageBuildKey),
}

func delivery(source event.Source, header, value string) event.Delivery {
	return event.Delivery{Source: source, Body: []byte("body"), Headers: map[string]string{header: value}}
}

func TestVerifyAcceptsEachSourceFormat(t *testing.T) {
	cases := []event.Delivery{
		delivery(event.SourceVikunja, VikunjaHeader, vikunjaSig),
		delivery(event.SourceGitHub, GitHubHeader, "sha256="+githubSig),
		delivery(event.SourceWikiComment, DispatchHeader, "sha256="+wikiCommentSig),
		delivery(event.SourceImageBuild, DispatchHeader, "sha256="+imageBuildSig),
	}
	for _, d := range cases {
		if err := keys.Verify(d); err != nil {
			t.Errorf("%s: %v", d.Source, err)
		}
	}
}

func TestVerifyRefusals(t *testing.T) {
	cases := map[string]struct {
		keys Keys
		d    event.Delivery
		want error
	}{
		"unsigned":                         {keys, delivery(event.SourceGitHub, "X-Other", "x"), ErrUnsigned},
		"wrong digest":                     {keys, delivery(event.SourceGitHub, GitHubHeader, "sha256="+strings.Repeat("0", 64)), ErrBadSignature},
		"github no prefix":                 {keys, delivery(event.SourceGitHub, GitHubHeader, githubSig), ErrBadSignature},
		"vikunja prefixed":                 {keys, delivery(event.SourceVikunja, VikunjaHeader, "sha256="+vikunjaSig), ErrBadSignature},
		"no key for source":                {Keys{event.SourceGitHub: []byte("kv")}, delivery(event.SourceVikunja, VikunjaHeader, vikunjaSig), ErrBadSignature},
		"github key on vikunja":            {keys, delivery(event.SourceVikunja, VikunjaHeader, githubSig), ErrBadSignature},
		"wiki-comment key on image-build":  {keys, delivery(event.SourceImageBuild, DispatchHeader, "sha256="+wikiCommentSig), ErrBadSignature},
		"github header on dispatch source": {keys, delivery(event.SourceWikiComment, GitHubHeader, "sha256="+wikiCommentSig), ErrUnsigned},
		"unknown source":                   {keys, delivery("cluster-triage", DispatchHeader, "sha256="+imageBuildSig), ErrUnknownSource},
	}
	for name, c := range cases {
		if err := c.keys.Verify(c.d); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
}

// sized is a delivery of n bytes signed under its source's key.
func sized(source event.Source, header, prefix, key string, n int) event.Delivery {
	body := []byte(strings.Repeat("x", n))
	return event.Delivery{Source: source, Body: body,
		Headers: map[string]string{header: prefix + Digest([]byte(key), body)}}
}

func TestVerifyBodyCapsPerSource(t *testing.T) {
	cases := map[string]struct {
		d    event.Delivery
		want error
	}{
		"vikunja over its cap":               {sized(event.SourceVikunja, VikunjaHeader, "", "kv", MaxVikunjaBodyBytes+1), ErrTooLarge},
		"github over the vikunja cap":        {sized(event.SourceGitHub, GitHubHeader, sha256Prefix, githubKey, MaxVikunjaBodyBytes+1), nil},
		"github over its cap":                {sized(event.SourceGitHub, GitHubHeader, sha256Prefix, githubKey, MaxGitHubBodyBytes+1), ErrTooLarge},
		"image-build at its cap":             {sized(event.SourceImageBuild, DispatchHeader, sha256Prefix, imageBuildKey, MaxDispatchBodyBytes), nil},
		"image-build over its cap":           {sized(event.SourceImageBuild, DispatchHeader, sha256Prefix, imageBuildKey, MaxDispatchBodyBytes+1), ErrTooLarge},
		"wiki-comment over the dispatch cap": {sized(event.SourceWikiComment, DispatchHeader, sha256Prefix, "kw", MaxDispatchBodyBytes+1), ErrTooLarge},
	}
	for name, c := range cases {
		if err := keys.Verify(c.d); !errors.Is(err, c.want) {
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
	if _, _, err := Sign(event.SourceGitHub, nil, []byte("body")); !errors.Is(err, ErrNoKey) {
		t.Errorf("empty key: %v", err)
	}
}
