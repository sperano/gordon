package notice

import (
	"errors"
	"testing"
)

func TestHoldRoundTrip(t *testing.T) {
	for _, h := range []Hold{
		{Run: "vikunja-7-gordon-sonnet", Reason: HoldLimit, Variant: "claude-sonnet", Running: 2, Limit: 2},
		{Run: "vikunja-fu-gordon-7-c9", Reason: HoldBranch, Branch: "kelos/vikunja-7", Holder: "vikunja-7-gordon-opus"},
	} {
		got, err := ParseHold(h.Detail())
		if err != nil || got != h {
			t.Fatalf("ParseHold(%q) = %+v, %v; want %+v", h.Detail(), got, err, h)
		}
	}
}

func TestHoldKeyLeavesTheCountOut(t *testing.T) {
	a := Hold{Run: "r", Reason: HoldLimit, Variant: "claude-sonnet", Running: 2, Limit: 2}
	b := a
	b.Running = 3
	if a.Key() != "r:limit:claude-sonnet:2" || b.Key() != a.Key() {
		t.Fatalf("keys %q %q", a.Key(), b.Key())
	}
	if a.Detail() != "r:limit:claude-sonnet:2:2" || b.Detail() != "r:limit:claude-sonnet:2:3" {
		t.Fatalf("details %q %q", a.Detail(), b.Detail())
	}
	br := Hold{Run: "r", Reason: HoldBranch, Branch: "kelos/x", Holder: "y"}
	if br.Key() != "r:branch:kelos/x:y" || br.Detail() != br.Key() {
		t.Fatalf("branch key %q detail %q", br.Key(), br.Detail())
	}
}

func TestHoldMessage(t *testing.T) {
	cases := map[string]Hold{
		"variant claude-sonnet runs 2 of 2": {Reason: HoldLimit, Variant: "claude-sonnet", Running: 2, Limit: 2},
		"branch kelos/x held by y":          {Reason: HoldBranch, Branch: "kelos/x", Holder: "y"},
	}
	for want, h := range cases {
		if got := h.Message(); got != want {
			t.Fatalf("message %q, want %q", got, want)
		}
	}
}

func TestParseHoldRefusals(t *testing.T) {
	for _, d := range []string{"", "r", ":limit:v:2:2", "r:limit:v:2", "r:limit::2:2", "r:limit:v:x:2",
		"r:limit:v:2:x", "r:branch:b", "r:branch::y", "r:branch:b:", "r:other:a:b", "r:branch:b:y:z"} {
		if _, err := ParseHold(d); !errors.Is(err, ErrHold) {
			t.Fatalf("ParseHold(%q) = %v, want ErrHold", d, err)
		}
	}
}

func TestQueuedText(t *testing.T) {
	limit := QueuedText(Hold{Run: "vikunja-7-gordon-sonnet", Reason: HoldLimit, Variant: "claude-sonnet", Running: 2, Limit: 2}, "queued")
	want := "<p>HOLD YOUR HORSES. KELOS TASK <code>vikunja-7-gordon-sonnet</code> WAITS IN LINE: " +
		"<code>claude-sonnet</code> ALREADY RUNS 2 OF ITS 2. IT STARTS BY ITSELF ONCE THAT CLEARS, AND THE " +
		"<code>queued</code> LABEL COMES OFF.</p><!-- kelos:queued task=vikunja-7-gordon-sonnet -->"
	if limit != want {
		t.Fatalf("limit text\n%s\nwant\n%s", limit, want)
	}
	branch := QueuedText(Hold{Run: "r", Reason: HoldBranch, Branch: "kelos/<x>", Holder: "y"}, "queued")
	want = "<p>HOLD YOUR HORSES. KELOS TASK <code>r</code> WAITS IN LINE: BRANCH <code>kelos/&lt;x&gt;</code> " +
		"IS HELD BY KELOS TASK <code>y</code>. IT STARTS BY ITSELF ONCE THAT CLEARS, AND THE " +
		"<code>queued</code> LABEL COMES OFF.</p><!-- kelos:queued task=r -->"
	if branch != want {
		t.Fatalf("branch text\n%s\nwant\n%s", branch, want)
	}
}
