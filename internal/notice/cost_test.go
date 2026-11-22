package notice

import (
	"strings"
	"testing"
)

func TestDollars(t *testing.T) {
	for in, want := range map[string]string{
		"18.886400": "$18.89", "0.004999": "UNDER $0.01", "0.005": "$0.01", "0": "$0.00", "0.000000": "$0.00",
		"1234.994999": "$1234.99", "2.5": "$2.50", "7": "$7.00",
	} {
		if got, ok := Dollars(in); !ok || got != want {
			t.Errorf("Dollars(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "-1.00", "1/3", "1e3", "abc"} {
		if got, ok := Dollars(in); ok {
			t.Errorf("Dollars(%q) = %q, want refused", in, got)
		}
	}
}

func TestAmount(t *testing.T) {
	for _, c := range []struct {
		e    Estimate
		want string
	}{
		{Estimate{Status: CostComplete, USD: "1.234"}, "$1.23"},
		{Estimate{Status: CostPartial, USD: "1.235"}, "AT LEAST $1.24"},
		{Estimate{Status: CostPartial}, "UNKNOWN"},
		{Estimate{Status: CostUnknown, USD: "3.00"}, "UNKNOWN"},
		{Estimate{Status: CostNone}, "NO MODEL USAGE"},
		{Estimate{Status: CostComplete, USD: "nan"}, "UNKNOWN"},
	} {
		if got := Amount(c.e); got != c.want {
			t.Errorf("Amount(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

const costRow = `<tr><td style="width: 1%; white-space: nowrap">`

func TestCostTextFinal(t *testing.T) {
	got := CostText(CostInfo{Task: "vikunja-gordon-claude-opus-1632", Known: true, Final: true,
		Run:   Estimate{Status: CostComplete, USD: "4.295000"},
		Total: Estimate{Status: CostPartial, USD: "18.886400", Notes: []string{"cache-write-ttl-unknown"}},
		Runs:  3,
		Categories: []CostCategory{{Category: "uncachedInput", Tokens: 10, USD: "1.000000"},
			{Category: "cacheRead", Tokens: 0}, {Category: "cacheWriteUnknownTTL", Tokens: 5, Unpriced: true},
			{Category: "output", Tokens: 3, USD: "3.295000"}, {Category: "toolCalls", Tokens: 1, USD: "0.1", Unpriced: true}},
		PriceVersion: "2026-11-22.1", PriceDate: "2026-11-22", Assumptions: []string{"api-equivalent", "list-prices"},
		RunURL: "https://lucarne.example/runs/c/kelos-agents/u-1", CostURL: "https://sillage.example/task/v.example/1632#cost"})
	want := "<p>THE TAB FOR RUN <code>vikunja-gordon-claude-opus-1632</code> IS IN.</p><table><tbody>" +
		costRow + "THIS RUN</td><td>$4.30</td></tr>" +
		costRow + "TASK TO DATE</td><td>AT LEAST $18.89 OVER 3 RUNS</td></tr>" +
		costRow + "THIS RUN BY CATEGORY</td><td>UNCACHED INPUT $1.00 · CACHE WRITES (TTL UNKNOWN) UNPRICED · " +
		"OUTPUT $3.30 · <code>toolCalls</code> AT LEAST $0.10</td></tr>" +
		costRow + "COVERAGE</td><td>RUN <code>complete</code>; TASK <code>partial</code> (<code>cache-write-ttl-unknown</code>)</td></tr>" +
		costRow + "PRICES</td><td><code>2026-11-22.1</code>, VERIFIED 2026-11-22</td></tr>" +
		costRow + "ASSUMPTIONS</td><td><code>api-equivalent</code>, <code>list-prices</code></td></tr>" +
		"</tbody></table>" +
		"<p>ESTIMATED API-EQUIVALENT COST AT THE PROVIDERS' PUBLIC LIST PRICES. NOT WHAT WAS BILLED, NOT MONEY SAVED.</p>" +
		`<p><a href="https://lucarne.example/runs/c/kelos-agents/u-1">lucarne</a> / ` +
		`<a href="https://sillage.example/task/v.example/1632#cost">sillage</a></p>` +
		"<!-- kelos:cost task=vikunja-gordon-claude-opus-1632 -->"
	if got != want {
		t.Errorf("CostText\n got %s\nwant %s", got, want)
	}
}

// TestCostTextUnknown: a failed run Sillage has not seen, of a task
// Sillage has no story of, posted at the deadline: everything unknown,
// marked not final, no zero anywhere.
func TestCostTextUnknown(t *testing.T) {
	got := CostText(CostInfo{Task: "vikunja-fu-gordon-5-c9", Failed: true,
		Run: Estimate{Status: CostUnknown}, Total: Estimate{Status: CostUnknown}})
	want := "<p>THE TAB FOR FAILED RUN <code>vikunja-fu-gordon-5-c9</code> IS IN.</p><table><tbody>" +
		costRow + "THIS RUN</td><td>UNKNOWN</td></tr>" +
		costRow + "TASK TO DATE</td><td>UNKNOWN</td></tr>" +
		costRow + "COVERAGE</td><td>RUN <code>unknown</code>; TASK <code>unknown</code></td></tr>" +
		costRow + "PRICES</td><td>UNKNOWN</td></tr>" +
		"</tbody></table>" +
		"<p>SILLAGE HAS NO RECORD OF THIS RUN'S USAGE YET.</p>" +
		"<p>NOT FINAL: MORE USAGE MAY STILL ARRIVE. I'LL UPDATE THIS NOTICE WHEN IT DOES.</p>" +
		"<p>ESTIMATED API-EQUIVALENT COST AT THE PROVIDERS' PUBLIC LIST PRICES. NOT WHAT WAS BILLED, NOT MONEY SAVED.</p>" +
		"<!-- kelos:cost task=vikunja-fu-gordon-5-c9 -->"
	if got != want {
		t.Errorf("CostText\n got %s\nwant %s", got, want)
	}
}

func TestCostTextOneRun(t *testing.T) {
	got := CostText(CostInfo{Task: "t", Known: true, Final: true, Runs: 1,
		Run: Estimate{Status: CostComplete, USD: "1"}, Total: Estimate{Status: CostComplete, USD: "1"},
		PriceVersion: "v", PriceDate: "d"})
	if want := costRow + "TASK TO DATE</td><td>$1.00 OVER 1 RUN</td></tr>"; !strings.Contains(got, want) {
		t.Errorf("CostText %s lacks %s", got, want)
	}
}

func TestCostRevision(t *testing.T) {
	text := "x" + CostRevisionMarker("other", 7, 9) + CostRevisionMarker("run-a", 8, 4) + CostRevisionMarker("run-a", 7, 3)
	if rev, ok := CostRevision(text, "run-a", 7); !ok || rev != 3 {
		t.Errorf("CostRevision = %d, %v, want 3", rev, ok)
	}
	if _, ok := CostRevision(text, "run-b", 7); ok {
		t.Error("another run's marker counted")
	}
	if _, ok := CostRevision("<!-- gordon:cost run=run-a gen=7 rev=99999999999999999999 -->", "run-a", 7); ok {
		t.Error("a marker with an overflowing revision counted")
	}
	if got := CostRevisionMarker("run-a", 7, 3); got != "<!-- gordon:cost run=run-a gen=7 rev=3 -->" {
		t.Errorf("marker %q", got)
	}
}
