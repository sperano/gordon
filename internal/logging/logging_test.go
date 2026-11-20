package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

func logged(t *testing.T, args ...any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo).Info("event", args...)
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("log line %q is not JSON: %v", buf.String(), err)
	}
	return out
}

func TestSensitiveKeysAreRedactedWhole(t *testing.T) {
	out := logged(t, "body", `{"comment":"/gordon do x"}`, "Authorization", "Bearer abc", "prompt", "p",
		"comment", "c", "token", "t", "signature", "s", "access_token", "a", "API_KEY", "k", "client_secret", "cs",
		KeyDelivery, "github/delivery/1")
	for _, k := range []string{"body", "Authorization", "prompt", "comment", "token", "signature", "access_token",
		"API_KEY", "client_secret"} {
		if out[k] != Redacted {
			t.Errorf("%s = %v, want %s", k, out[k], Redacted)
		}
	}
	if out[KeyDelivery] != "github/delivery/1" {
		t.Errorf("correlation key altered: %v", out[KeyDelivery])
	}
}

func TestScrubMasksTokensInValuesAndErrors(t *testing.T) {
	cases := map[string]string{
		"get: Bearer abcdefghij123 refused":                                           "get: " + Redacted + " refused",
		"token ghp_" + strings.Repeat("a", 36) + " invalid":                           "token " + Redacted + " invalid",
		"pat github_pat_" + strings.Repeat("B_", 15) + " x":                           "pat " + Redacted + " x",
		"sig sha256=" + strings.Repeat("ab", 32):                                      "sig " + Redacted,
		"dial postgres://gordon:hunter2@db:5432/gordon failed":                        "dial postgres://gordon:" + Redacted + "@db:5432/gordon failed",
		"url ?password=x&a=1":                                                         "url ?" + Redacted + "&a=1",
		"url ?access_token=x&client_secret=y&a=1":                                     "url ?" + Redacted + "&" + Redacted + "&a=1",
		"dial postgres://:hunter2@db/gordon":                                          "dial postgres://:" + Redacted + "@db/gordon",
		"key -----BEGIN RSA PRIVATE KEY-----\nMII\n-----END RSA PRIVATE KEY----- end": "key " + Redacted + " end",
		"auth Basic dXNlcjpwYXNz rejected":                                            "auth " + Redacted + " rejected",
		"token abcdefghijklmnopqrstuvwxyz end":                                        Redacted + " end",
		"basic authentication failed":                                                 "basic authentication failed",
		"token refreshing for vikunja":                                                "token refreshing for vikunja",
		"Bearer Authorization header missing":                                         "Bearer Authorization header missing",
	}
	for in, want := range cases {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
	out := logged(t, "error", errors.New("vikunja: Bearer s3cr3tvalue1 rejected"))
	if out["error"] != "vikunja: "+Redacted+" rejected" {
		t.Errorf("error attr = %v, want the token masked", out["error"])
	}
}

func TestScrubTruncatesOnRuneBoundary(t *testing.T) {
	// "x" then two-byte runes: byte MaxValueBytes is a continuation byte,
	// so a plain s[:MaxValueBytes] would split a rune.
	in := "x" + strings.Repeat("é", MaxValueBytes)
	got := Scrub(in)
	body, ok := strings.CutSuffix(got, truncatedSuffix)
	if !ok {
		t.Fatalf("Scrub(%d bytes) has no truncation marker", len(in))
	}
	if !utf8.ValidString(body) || len(body) != MaxValueBytes-1 || body != in[:MaxValueBytes-1] {
		t.Fatalf("kept %d bytes (valid UTF-8 %v), want the %d bytes before the split rune",
			len(body), utf8.ValidString(body), MaxValueBytes-1)
	}
}

func TestParseLevel(t *testing.T) {
	if l, err := ParseLevel("warn"); err != nil || l != slog.LevelWarn {
		t.Fatalf("ParseLevel(warn) = %v, %v", l, err)
	}
	if _, err := ParseLevel("loud"); err == nil || !strings.Contains(err.Error(), `"loud"`) {
		t.Fatalf("ParseLevel(loud) = %v, want a refusal naming it", err)
	}
}

func TestStructuredValuesAreScrubbed(t *testing.T) {
	out := logged(t, "runs", []string{"run-a", "run-b"},
		"urls", []string{"https://x/?access_token=s3cr3t"}, "big", []string{strings.Repeat("x", MaxValueBytes)})
	runs, ok := out["runs"].([]any)
	if !ok || len(runs) != 2 || runs[0] != "run-a" || runs[1] != "run-b" {
		t.Errorf("clean structured value = %#v, want it kept as is", out["runs"])
	}
	if out["urls"] != `["https://x/?`+Redacted { // the mask runs to the next space or &
		t.Errorf("structured value with a credential = %#v, want its scrubbed JSON", out["urls"])
	}
	if big, _ := out["big"].(string); !strings.HasSuffix(big, truncatedSuffix) {
		t.Errorf("oversized structured value not truncated: %d bytes", len(big))
	}
}
