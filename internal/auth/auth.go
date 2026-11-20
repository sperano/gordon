// Package auth authenticates webhook deliveries by source, before any body
// is parsed. Each source has its own key and signature header, so a holder
// of one key cannot sign for another source; the formats are those the
// legacy receivers check (hollingsworth helm/kelos/scripts/
// dispatch_signature.py, github_reactor.py), so a source can be pointed at
// Gordon without changing how it signs.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/sperano/gordon/internal/event"
)

// Signature headers and prefixes by source.
const (
	VikunjaHeader  = "X-Vikunja-Signature"
	GitHubHeader   = "X-Hub-Signature-256"
	DispatchHeader = "X-Kelos-Signature-256"
	sha256Prefix   = "sha256="
)

// Body limits by source, the legacy receivers' caps.
const (
	MaxVikunjaBodyBytes  = 10 << 20
	MaxGitHubBodyBytes   = 25 << 20
	MaxDispatchBodyBytes = 10 << 20
)

// Refusals, each with the HTTP status a receiver answers.
var (
	ErrUnsigned      = errors.New("signature missing")
	ErrBadSignature  = errors.New("signature invalid")
	ErrUnknownSource = errors.New("unknown source")
	ErrTooLarge      = errors.New("body too large")
	// ErrNoKey refuses to sign with an empty key, which Verify never
	// accepts.
	ErrNoKey = errors.New("signing key empty")
)

// Status is the HTTP status a receiver answers for a refusal, as the
// legacy receivers do (401 unsigned, 403 wrong, 404 unknown source, 413
// too large); 0 for nil.
func Status(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrUnsigned):
		return http.StatusUnauthorized
	case errors.Is(err, ErrBadSignature):
		return http.StatusForbidden
	case errors.Is(err, ErrUnknownSource):
		return http.StatusNotFound
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusBadRequest
	}
}

type scheme struct {
	header   string
	prefix   string
	maxBytes int
}

var schemes = map[event.Source]scheme{
	event.SourceVikunja:     {VikunjaHeader, "", MaxVikunjaBodyBytes},
	event.SourceGitHub:      {GitHubHeader, sha256Prefix, MaxGitHubBodyBytes},
	event.SourceWikiComment: {DispatchHeader, sha256Prefix, MaxDispatchBodyBytes},
	event.SourceImageBuild:  {DispatchHeader, sha256Prefix, MaxDispatchBodyBytes},
}

// Keys holds each source's signing key. A source without a key accepts
// nothing.
type Keys map[event.Source][]byte

// Verify returns nil when the delivery is signed under its source's key.
func (k Keys) Verify(d event.Delivery) error {
	s, ok := schemes[d.Source]
	if !ok {
		return ErrUnknownSource
	}
	if len(d.Body) > s.maxBytes {
		return ErrTooLarge
	}
	got := d.Header(s.header)
	if got == "" {
		return ErrUnsigned
	}
	key := k[d.Source]
	if len(key) == 0 || !strings.HasPrefix(got, s.prefix) {
		return ErrBadSignature
	}
	want := Digest(key, d.Body)
	if !hmac.Equal([]byte(got[len(s.prefix):]), []byte(want)) {
		return ErrBadSignature
	}
	return nil
}

// MaxBodyBytes is the body cap of source; false for an unknown source.
func MaxBodyBytes(source event.Source) (int, bool) {
	s, ok := schemes[source]
	return s.maxBytes, ok
}

// Header is the signature header of source; false for an unknown source.
func Header(source event.Source) (string, bool) {
	s, ok := schemes[source]
	return s.header, ok
}

// Digest is the hex HMAC-SHA256 of body under key.
func Digest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign returns the header name and value that sign body for source; an
// empty key is refused, as Verify refuses it.
func Sign(source event.Source, key, body []byte) (header, value string, err error) {
	s, ok := schemes[source]
	if !ok {
		return "", "", ErrUnknownSource
	}
	if len(key) == 0 {
		return "", "", ErrNoKey
	}
	return s.header, s.prefix + Digest(key, body), nil
}
