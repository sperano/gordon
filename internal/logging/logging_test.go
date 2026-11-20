package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
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
		KeyDelivery, "github/delivery/1")
	for _, k := range []string{"body", "Authorization", "prompt"} {
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
		"key -----BEGIN RSA PRIVATE KEY-----\nMII\n-----END RSA PRIVATE KEY----- end": "key " + Redacted + " end",
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
	in := strings.Repeat("é", MaxValueBytes) // two bytes each
	got := Scrub(in)
	if !strings.HasSuffix(got, truncatedSuffix) || len(got) > MaxValueBytes+len(truncatedSuffix) {
		t.Fatalf("Scrub kept %d bytes, want at most %d plus the marker", len(got), MaxValueBytes)
	}
	if body := strings.TrimSuffix(got, truncatedSuffix); strings.ContainsRune(body, '�') || len(body)%2 != 0 {
		t.Fatalf("truncation split a rune: %d bytes", len(body))
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
