package notice

import (
	"encoding/json"
	"os"
	"testing"
)

// readinessTexts are the exact readiness notice texts, first printed by
// the scripts Gordon replaced (hollingsworth 24a3c11, helm/kelos/scripts:
// reconcile_pr_ready.py, review_findings.py, missing_checks.py,
// auto_merge.py and pr_ready_notify.py).
func readinessTexts(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("testdata/readiness_texts.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const sampleSHA = "0123456789abcdef0123456789abcdef01234567"

func gordonHead(extra string) Head {
	return Head{Repo: "sperano/gordon", Number: 12, SHA: sampleSHA, Extra: extra}
}

func TestReadinessTextsGolden(t *testing.T) {
	want := readinessTexts(t)
	must := func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	vigie, nhl := gordonHead(MissingExtra([]string{"check"})), gordonHead(MissingExtra([]string{"Format", "Test <default>"}))
	vigie.Repo, nhl.Repo = "sperano/vigie", "sperano/nhl"
	refusal := `PUT https://api.github.com/repos/sperano/gordon/pulls/12/merge: HTTP 405 Method Not Allowed: ` +
		`{"message":"Pull Request is not mergeable","status":"405"}`
	got := map[string]string{
		"ready":         ReadyText(gordonHead("")),
		"ready_escaped": ReadyText(Head{Repo: "o/r&x", Number: 3, SHA: "fedcba9876543210"}),
		"round_limit_one": must(RoundLimitText(gordonHead(LimitedExtra([]Limited{
			{Author: "gordon-cole-ai[bot]", Review: 99}})))),
		"round_limit_many": must(RoundLimitText(gordonHead(LimitedExtra([]Limited{
			{Author: "cubic-dev-ai[bot]", Review: 7}, {Author: "gordon-cole-ai[bot]", Review: 99}})))),
		"checks_missing_one":  must(ChecksMissingText(vigie, 30)),
		"checks_missing_many": must(ChecksMissingText(nhl, 45)),
		"merge_refused":       MergeRefusedText(gordonHead(refusal)),
	}
	for k, g := range got {
		w, ok := want[k]
		if !ok {
			t.Errorf("%s: no golden text", k)
			continue
		}
		if g != w {
			t.Errorf("%s:\n got %q\nwant %q", k, g, w)
		}
	}
}

func TestReadinessSignaturesGolden(t *testing.T) {
	want := readinessTexts(t)
	for _, c := range []struct {
		name, kind string
		h          Head
		text       string
	}{
		{"ready_signature", Ready, gordonHead(""), ReadyText(gordonHead(""))},
		{"round_limit_signature", RoundLimit, gordonHead(""), want["round_limit_one"]},
		{"checks_missing_signature", ChecksMissing, Head{Repo: "sperano/vigie", Number: 12, SHA: sampleSHA},
			want["checks_missing_one"]},
		{"merge_refused_signature", MergeRefused, gordonHead(""), want["merge_refused"]},
	} {
		if got := c.h.signature(headings[c.kind]); got != want[c.name] {
			t.Errorf("%s = %q, want %q", c.name, got, want[c.name])
		}
		if !HeadPosted(c.text, c.kind, c.h) {
			t.Errorf("%s: HeadPosted(golden text) = false", c.name)
		}
		other := c.h
		other.SHA = "fedcba9876543210"
		if HeadPosted(c.text, c.kind, other) {
			t.Errorf("%s: HeadPosted for another head = true", c.name)
		}
	}
}

func TestHeadPostedNeedsTheKindAndVisibleText(t *testing.T) {
	h := gordonHead("")
	sig := "READY TO MERGE: sperano/gordon#12. HEAD 01234567."
	cases := []struct {
		name, text, kind string
		want             bool
	}{
		{"plain text", "<p>" + sig + "</p>", Ready, true},
		{"split by markup", "<p>READY TO MERGE: <b>sperano/gordon#12</b>.\n HEAD <code>01234567</code>.</p>", Ready, true},
		{"only in a hidden comment", "<!-- " + sig + " -->", Ready, false},
		{"another kind's heading", "<p>" + sig + "</p>", RoundLimit, false},
		{"unknown kind", "<p>" + sig + "</p>", "pr-open", false},
	}
	for _, c := range cases {
		if got := HeadPosted(c.text, c.kind, h); got != c.want {
			t.Errorf("%s: HeadPosted = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHeadDetailRoundTrips(t *testing.T) {
	for _, h := range []Head{gordonHead(""), gordonHead(`["a b", "c"]`), gordonHead("PUT x: HTTP 409 Conflict")} {
		got, err := ParseHead(h.Detail())
		if err != nil || got != h {
			t.Errorf("ParseHead(%q) = %+v, %v; want %+v", h.Detail(), got, err, h)
		}
	}
	for _, bad := range []string{"", "sperano/gordon#12", "sperano/gordon#0@abc", "gordon#12@abc", "sperano/gordon#12@XYZ",
		"a/b/c#1@abc"} {
		if _, err := ParseHead(bad); err == nil {
			t.Errorf("ParseHead(%q) accepted", bad)
		}
	}
}

func TestReadinessTextsRefuseMissingPayloads(t *testing.T) {
	for _, extra := range []string{"", "[]", "not json"} {
		if _, err := RoundLimitText(gordonHead(extra)); err == nil {
			t.Errorf("RoundLimitText(%q) accepted", extra)
		}
		if _, err := ChecksMissingText(gordonHead(extra), 1); err == nil {
			t.Errorf("ChecksMissingText(%q) accepted", extra)
		}
	}
}
