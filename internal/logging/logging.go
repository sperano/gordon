// Package logging builds Gordon's structured JSON logger for stdout. It
// redacts before anything is written: attributes whose key names a secret
// or user text (a body, a comment, a prompt) are replaced whole, and
// token-shaped substrings in every other string, error included, are
// masked. Long values are truncated, so one delivery cannot flood the log.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces a redacted value.
const Redacted = "[redacted]"

// MaxValueBytes caps one logged string value.
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
	KeyRun      = "run"
	KeyTask     = "task"
)

// sensitiveKeys are attribute keys whose value is never logged: secrets,
// and user-written text that may carry a prompt or a pasted secret.
var sensitiveKeys = map[string]bool{
	"body": true, "comment": true, "prompt": true, "text": true, "payload": true,
	"token": true, "secret": true, "password": true, "authorization": true,
	"signature": true, "cookie": true, "dsn": true, "database_url": true,
}

// secretPatterns match credentials that may appear inside other values
// (an error quoting a URL or a header).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	regexp.MustCompile(`(?i)\b(bearer|token|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`sha256=[0-9a-fA-F]{16,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api_key|apikey)=[^\s&]+`),
}

// userinfo matches the password of a URL's userinfo.
var userinfo = regexp.MustCompile(`(://[^/\s:@]+):[^@\s/]+@`)

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
	if sensitiveKeys[strings.ToLower(a.Key)] {
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
	}
	return a
}

// Scrub masks credentials in s and truncates it to MaxValueBytes.
func Scrub(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
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
