package notice

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Cost is the kind of a run's completion cost notice (Gordon 12).
const Cost = "cost"

// Statuses of an estimate, as Sillage names them.
const (
	CostComplete = "complete"
	CostPartial  = "partial"
	CostUnknown  = "unknown"
	CostNone     = "none"
)

// centsPerDollar rounds an estimate to the cent it is shown with.
const centsPerDollar = 100

// costRevision is the marker of a cost notice's generation and revision:
// the notice of run <name> of generation <gen> at revision <rev>. A later
// revision edits the same comment; a comment at the revision or a later
// one needs no write.
var costRevision = regexp.MustCompile(`<!-- gordon:cost run=([A-Za-z0-9][A-Za-z0-9._-]*) gen=(\d+) rev=(\d+) -->`)

// CostRevisionMarker marks a cost notice's comment with its generation
// and revision.
func CostRevisionMarker(name string, generation int64, rev int) string {
	return fmt.Sprintf("<!-- gordon:cost run=%s gen=%d rev=%d -->", name, generation, rev)
}

// CostRevision is the revision of the cost notice of run name's
// generation that text carries, false when it carries none.
func CostRevision(text, name string, generation int64) (int, bool) {
	for _, m := range costRevision.FindAllStringSubmatch(text, -1) {
		gen, err := strconv.ParseInt(m[2], 10, 64)
		if m[1] != name || err != nil || gen != generation {
			continue
		}
		if rev, err := strconv.Atoi(m[3]); err == nil {
			return rev, true
		}
	}
	return 0, false
}

// Estimate is one estimate as a notice shows it.
type Estimate struct {
	Status string
	// USD is Sillage's decimal string; "" when nothing was priced.
	USD   string
	Notes []string
}

// CostCategory is one category of a run's estimate.
type CostCategory struct {
	Category string
	Tokens   int64
	USD      string
	Unpriced bool
}

// CostInfo is what a completion cost notice shows.
type CostInfo struct {
	Task   string
	Failed bool
	// Known: Sillage reported the run; Final: no more usage is expected.
	Known, Final bool
	Run          Estimate
	Total        Estimate
	// Runs is how many runs the task total covers.
	Runs                    int
	Categories              []CostCategory
	PriceVersion, PriceDate string
	Assumptions             []string
	RunURL, CostURL         string
}

// categoryNames are Sillage's categories as a notice names them.
var categoryNames = map[string]string{
	"uncachedInput":        "UNCACHED INPUT",
	"cacheRead":            "CACHE READS",
	"cacheWrite5m":         "CACHE WRITES (5M)",
	"cacheWrite1h":         "CACHE WRITES (1H)",
	"cacheWriteUnknownTTL": "CACHE WRITES (TTL UNKNOWN)",
	"output":               "OUTPUT",
}

const (
	costCloser = "ESTIMATED API-EQUIVALENT COST AT THE PROVIDERS' PUBLIC LIST PRICES. NOT WHAT WAS BILLED, NOT MONEY SAVED."
	notFinal   = "NOT FINAL: MORE USAGE MAY STILL ARRIVE. I'LL UPDATE THIS NOTICE WHEN IT DOES."
	runUnseen  = "SILLAGE HAS NO RECORD OF THIS RUN'S USAGE YET."
)

// CostText is a run's completion cost notice: the run's estimate, the
// task's total to date, the run's categories, its coverage, the price
// table and assumptions, and the Lucarne and Sillage links. A missing
// estimate is UNKNOWN, a partial one AT LEAST, never a zero.
func CostText(c CostInfo) string {
	var b strings.Builder
	what := "RUN"
	if c.Failed {
		what = "FAILED RUN"
	}
	fmt.Fprintf(&b, "<p>THE TAB FOR %s %s IS IN.</p><table><tbody>", what, code(c.Task))
	total := Amount(c.Total)
	if c.Runs > 0 {
		total += fmt.Sprintf(" OVER %d %s", c.Runs, plural(c.Runs, "RUN", "RUNS"))
	}
	rows := [][2]string{{"THIS RUN", Amount(c.Run)}, {"TASK TO DATE", total}}
	if cats := categoryLine(c.Categories); cats != "" {
		rows = append(rows, [2]string{"THIS RUN BY CATEGORY", cats})
	}
	prices := "UNKNOWN"
	if c.PriceVersion != "" {
		prices = code(c.PriceVersion) + ", VERIFIED " + escape(c.PriceDate)
	}
	rows = append(rows, [2]string{"COVERAGE", "RUN " + coverage(c.Run) + "; TASK " + coverage(c.Total)},
		[2]string{"PRICES", prices})
	if len(c.Assumptions) > 0 {
		rows = append(rows, [2]string{"ASSUMPTIONS", codes(c.Assumptions)})
	}
	for _, row := range rows {
		fmt.Fprintf(&b, `<tr><td style="%s">%s</td><td>%s</td></tr>`, labelCell, row[0], row[1])
	}
	b.WriteString("</tbody></table>")
	if !c.Known {
		b.WriteString("<p>" + runUnseen + "</p>")
	}
	if !c.Final {
		b.WriteString("<p>" + notFinal + "</p>")
	}
	b.WriteString("<p>" + costCloser + "</p>")
	b.WriteString(links(c.RunURL, c.CostURL))
	return b.String() + Marker(Cost, c.Task)
}

// Amount is an estimate as shown: NO MODEL USAGE, UNKNOWN, AT LEAST $x
// (partial) or $x.
func Amount(e Estimate) string {
	switch {
	case e.Status == CostNone:
		return "NO MODEL USAGE"
	case e.Status == CostUnknown || e.USD == "":
		return "UNKNOWN"
	}
	usd, ok := Dollars(e.USD)
	if !ok {
		return "UNKNOWN"
	}
	if e.Status == CostPartial {
		return "AT LEAST " + usd
	}
	return usd
}

// Dollars is a decimal amount rounded half up to the cent ($1.24), one
// that rounds to nothing but is not zero UNDER $0.01; false for what is
// not a non-negative decimal.
func Dollars(s string) (string, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || strings.ContainsAny(s, "/eE") {
		return "", false
	}
	cents := new(big.Rat).Mul(r, big.NewRat(centsPerDollar, 1))
	cents.Add(cents, big.NewRat(1, 2))
	n := new(big.Int).Quo(cents.Num(), cents.Denom())
	if n.Sign() == 0 && r.Sign() > 0 {
		return "UNDER $0.01", true
	}
	whole, frac := new(big.Int).QuoRem(n, big.NewInt(centsPerDollar), new(big.Int))
	return fmt.Sprintf("$%s.%02d", whole.String(), frac.Int64()), true
}

func coverage(e Estimate) string {
	out := code(e.Status)
	if len(e.Notes) > 0 {
		out += " (" + codes(e.Notes) + ")"
	}
	return out
}

func categoryLine(cats []CostCategory) string {
	var parts []string
	for _, c := range cats {
		if c.Tokens <= 0 {
			continue
		}
		name, ok := categoryNames[c.Category]
		if !ok {
			name = code(c.Category)
		}
		status := CostComplete
		if c.Unpriced {
			status = CostPartial
		}
		amount := Amount(Estimate{Status: status, USD: c.USD})
		if c.USD == "" && c.Unpriced {
			amount = "UNPRICED"
		}
		parts = append(parts, name+" "+amount)
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
