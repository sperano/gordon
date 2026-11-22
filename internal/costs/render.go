package costs

import (
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/sillage"
	"github.com/sperano/gordon/internal/store"
)

// costAnchor is the fragment of Sillage's task page that shows the cost
// breakdown.
const costAnchor = "#cost"

// Render is the notice of run r (known: Sillage reported it) from its
// task's cost c. The run's own figures come from its run entry, which
// Sillage counts once whatever its attempts, models and subagent
// threads; the task total is Sillage's, never a sum Gordon makes. A
// task Sillage has no story of yet (c empty) is unknown throughout.
func Render(n store.CostNotice, c sillage.TaskCost, r sillage.Run, known bool, links Links) string {
	info := notice.CostInfo{Task: n.Name, Failed: n.Phase == kelos.PhaseFailed, Known: known,
		Final: known && r.Final, Runs: len(c.Runs),
		Run:          notice.Estimate{Status: notice.CostUnknown},
		Total:        estimate(c.Total),
		PriceVersion: c.Prices.Version, PriceDate: c.Prices.VerifiedAt,
		RunURL: links.Run(n.TaskUID)}
	if info.Total.Status == "" {
		info.Total.Status = notice.CostUnknown
	}
	if known {
		info.Run = estimate(r.Breakdown)
		for _, cat := range r.Categories {
			info.Categories = append(info.Categories, notice.CostCategory{Category: cat.Category, Tokens: cat.Tokens,
				USD: cat.EstimatedUSD, Unpriced: cat.Unpriced})
		}
	}
	for _, a := range c.Assumptions {
		info.Assumptions = append(info.Assumptions, a.Code)
	}
	if u := links.Activity(n.TaskID); u != "" {
		info.CostURL = u + costAnchor
	}
	return notice.CostText(info)
}

func estimate(b sillage.Breakdown) notice.Estimate {
	return notice.Estimate{Status: b.Status, USD: b.EstimatedUSD, Notes: b.Notes}
}
