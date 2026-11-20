// Package logging builds Gordon's structured JSON logger for stdout. It
// redacts before anything is written: attributes whose key names a secret
// or user text (a body, a comment, a prompt) are replaced whole, and
// token-shaped substrings in every other string, error included, are
// masked. Long values are truncated, so one delivery cannot flood the log.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces a redacted value.
const Redacted = "[redacted]"

// MaxValueBytes is where a long logged string value is cut; the
// truncation marker is appended after the cut.
const MaxValueBytes = 1024

// truncatedSuffix marks a value cut at MaxValueBytes.
const truncatedSuffix = "…[truncated]"

// Correlation attribute keys, shared by every component.
const (
	KeyInbox    = "inbox_id"
	KeyDelivery = "delivery_key"
	KeyRequest  = "request_key"
	KeySource   = "source"
	KeyKind     = "kind"
	KeyAction   = "action_key"
	KeyActionID = "action_id"
)

// sensitiveKeys are attribute keys whose value is never logged: secrets,
// and user-written text that may carry a prompt or a pasted secret.
var sensitiveKeys = map[string]bool{
	"body": true, "comment": true, "prompt": true, "text": true, "payload": true,
	"token": true, "secret": true, "password": true, "authorization": true,
	"signature": true, "cookie": true, "dsn": true, "database_url": true,
}

// sensitiveSuffixes redact a credential key by its ending (access_token,
// client_secret, api_key, db_password).
var sensitiveSuffixes = []string{"token", "secret", "password", "apikey", "api_key"}

func sensitive(key string) bool {
	k := strings.ToLower(key)
	if sensitiveKeys[k] {
		return true
	}
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// secretPatterns match credentials that may appear inside other values
// (an error quoting a URL or a header).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`sha256=[0-9a-fA-F]{16,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	// A credential parameter, prefixed or not (access_token, client_secret).
	regexp.MustCompile(`(?i)\b\w*(password|passwd|pwd|secret|token|api_key|apikey)=[^\s&]+`),
}

// schemeCredential is an authorization scheme word and the value after
// it; the value is masked only when it is credential-shaped (see
// credentialShaped), so "basic authentication failed" stays readable.
var schemeCredential = regexp.MustCompile(`(?i)\b(bearer|token|basic)(\s+)([A-Za-z0-9._~+/=-]{8,})`)

// minPlainCredential is the length from which a value after a scheme word
// is masked even when it is all lowercase letters.
const minPlainCredential = 20

// credentialShaped reports a value that looks generated rather than a
// word: long, or with a digit, a symbol, or an uppercase letter after its
// first.
func credentialShaped(v string) bool {
	if len(v) >= minPlainCredential {
		return true
	}
	for i, c := range v {
		switch {
		case c >= '0' && c <= '9', strings.ContainsRune("._~+/=-", c):
			return true
		case i > 0 && c >= 'A' && c <= 'Z':
			return true
		}
	}
	return false
}

func maskSchemeCredential(m string) string {
	parts := schemeCredential.FindStringSubmatch(m)
	if !credentialShaped(parts[3]) {
		return m
	}
	return Redacted
}

// userinfo matches the password of a URL's userinfo.
var userinfo = regexp.MustCompile(`(://[^/\s:@]*):[^@\s/]+@`)

// New returns a JSON logger writing to w at level.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: redact}))
}

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, fmt.Errorf("log level %q: want debug, info, warn or error", s)
	}
	return l, nil
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if sensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Scrub(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, Scrub(err.Error()))
		}
		if s, ok := a.Value.Any().(fmt.Stringer); ok {
			return slog.String(a.Key, Scrub(s.String()))
		}
		return scrubStructured(a)
	}
	return a
}

// scrubStructured checks a structured value (a slice, a map, a struct)
// through its JSON form: unchanged when Scrub leaves that form alone,
// otherwise logged as the scrubbed string, so no credential or oversized
// value passes inside one.
func scrubStructured(a slog.Attr) slog.Attr {
	b, err := json.Marshal(a.Value.Any())
	if err != nil {
		return slog.String(a.Key, Scrub(fmt.Sprint(a.Value.Any())))
	}
	if clean := Scrub(string(b)); clean != string(b) {
		return slog.String(a.Key, clean)
	}
	return a
}

// Scrub masks credentials in s and cuts it at MaxValueBytes (on a rune
// boundary), appending the truncation marker.
func Scrub(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
	s = schemeCredential.ReplaceAllStringFunc(s, maskSchemeCredential)
	s = userinfo.ReplaceAllString(s, "$1:"+Redacted+"@")
	if len(s) > MaxValueBytes {
		cut := MaxValueBytes
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + truncatedSuffix
	}
	return s
}

// utf8Start reports whether b starts a UTF-8 sequence.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
