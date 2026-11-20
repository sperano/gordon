package notice

import "testing"

func TestPullRequestTextsEqualLegacy(t *testing.T) {
	l := legacy(t)
	url, err := PRURL("sperano/vigie#4")
	if err != nil || url != "https://github.com/sperano/vigie/pull/4" {
		t.Fatalf("PRURL: %q, %v", url, err)
	}
	for _, c := range []struct{ key, got string }{
		{"pr_open", PROpenText(url, false)},
		{"pr_open_ready", PROpenText(url, true)},
		{"merged", MergedText(url)},
	} {
		if w := want(t, l, c.key); c.got != w {
			t.Errorf("%s:\n got %s\nwant %s", c.key, c.got, w)
		}
	}
}

func TestPRURLRefusesMalformedReferences(t *testing.T) {
	for _, ref := range []string{"", "vigie#4", "sperano/vigie", "sperano/vigie#x", "sperano/vigie#0", "a/b/c#4"} {
		if url, err := PRURL(ref); err == nil {
			t.Errorf("PRURL(%q) = %q, want an error", ref, url)
		}
	}
}

func TestPullRequestNoticesPosted(t *testing.T) {
	url := "https://github.com/sperano/vigie/pull/4"
	other := "https://github.com/sperano/vigie/pull/5"
	if !PROpenPosted(PROpenText(url, true), url) || PROpenPosted(PROpenText(other, false), url) {
		t.Error("PROpenPosted must match only the PR's own creation notice")
	}
	cases := []struct {
		text string
		want bool
	}{
		{MergedText(url), true},
		{MergedText(other), false},
		{`<p>Merged: <a href="` + url + `">#4</a></p>`, true},
		{`<p>Ready: <a href="` + url + `">#4</a></p>`, false},
		{`<p>Merged: <a href="` + url + `">#4</a></p>` + Marker(PROpen, ""), false},
	}
	for _, c := range cases {
		if got := MergedPosted(c.text, url); got != c.want {
			t.Errorf("MergedPosted(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
