package rollout

import (
	"strings"
	"testing"

	"github.com/sperano/gordon/internal/run"
)

var allGates = []Gate{ReplayParity, ShadowParity, RollbackTested, LegacyDisabled}

func TestZeroTableIsLegacy(t *testing.T) {
	var table Table
	for _, f := range run.Families {
		if got := table.Owner("hollingsworth", f); got != Legacy {
			t.Errorf("%s owner %s, want legacy", f, got)
		}
		if table.Executes("hollingsworth", f) {
			t.Errorf("%s executes in the zero table", f)
		}
	}
	if err := table.Validate(); err != nil {
		t.Errorf("zero table: %v", err)
	}
}

func TestProjectRuleWinsOverAllProjects(t *testing.T) {
	table := Table{Rules: []Rule{
		{Project: "vigie", Family: run.VikunjaImplement, Owner: Gordon, Evidence: allGates},
		{Project: AllProjects, Family: run.VikunjaImplement, Owner: Shadow, Evidence: []Gate{ReplayParity}},
	}}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		project string
		family  run.Family
		want    Owner
	}{
		{"vigie", run.VikunjaImplement, Gordon},
		{"sillage", run.VikunjaImplement, Shadow},
		{"vigie", run.PRReview, Legacy},
	}
	for _, c := range cases {
		if got := table.Owner(c.project, c.family); got != c.want {
			t.Errorf("%s/%s owner %s, want %s", c.project, c.family, got, c.want)
		}
	}
	if !table.Executes("vigie", run.VikunjaImplement) || table.Executes("sillage", run.VikunjaImplement) {
		t.Error("only the Gordon-owned scope executes")
	}
}

func TestValidateRefusals(t *testing.T) {
	cases := map[string]struct {
		rules []Rule
		want  string
	}{
		"unknown family": {[]Rule{{Project: AllProjects, Family: "wiki-followup", Owner: Legacy}}, "unknown family"},
		"unknown owner":  {[]Rule{{Project: AllProjects, Family: run.PRReview, Owner: "both"}}, "unknown owner"},
		"empty project":  {[]Rule{{Family: run.PRReview, Owner: Legacy}}, "empty project"},
		"duplicate": {[]Rule{{Project: "vigie", Family: run.PRReview, Owner: Legacy},
			{Project: "vigie", Family: run.PRReview, Owner: Legacy}}, "duplicate scope"},
		"shadow without replay": {[]Rule{{Project: "vigie", Family: run.PRReview, Owner: Shadow}}, "lacks gate replay-parity"},
		"gordon without legacy switch off": {[]Rule{{Project: "vigie", Family: run.PRReview, Owner: Gordon,
			Evidence: []Gate{ReplayParity, ShadowParity, RollbackTested}}}, "lacks gate legacy-disabled"},
	}
	for name, c := range cases {
		err := Table{Rules: c.rules}.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}
