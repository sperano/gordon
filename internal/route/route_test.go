package route_test

import (
	"encoding/json"
	"testing"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/replay"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

var key = []byte("route-test-key")

func router() route.Router {
	return route.Router{Config: profile.Defaults(), Keys: auth.Keys{
		event.SourceVikunja: key, event.SourceGitHub: key, event.SourceWikiComment: key, event.SourceImageBuild: key}}
}

func signed(t *testing.T, source event.Source, headers map[string]string, body any) event.Delivery {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	h, v, err := auth.Sign(source, key, raw)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{h: v}
	for k, val := range headers {
		all[k] = val
	}
	return event.Delivery{Source: source, Headers: all, Body: raw}
}

// family returns the decision's only family decision.
func family(t *testing.T, d route.Decision) route.FamilyDecision {
	t.Helper()
	if len(d.Families) != 1 {
		t.Fatalf("%d families in %+v, want 1", len(d.Families), d)
	}
	return d.Families[0]
}

func expect(t *testing.T, d route.Decision, outcome route.Outcome, reason string) {
	t.Helper()
	if d.Outcome != outcome || d.Reason != reason {
		t.Errorf("decision %s/%q, want %s/%q", d.Outcome, d.Reason, outcome, reason)
	}
}

func expectFamily(t *testing.T, fd route.FamilyDecision, f run.Family, outcome route.Outcome, reason string) {
	t.Helper()
	if fd.Family != f || fd.Outcome != outcome || fd.Reason != reason {
		t.Errorf("family %s %s/%q, want %s %s/%q", fd.Family, fd.Outcome, fd.Reason, f, outcome, reason)
	}
}

func keys(actions []action.Action) []string {
	out := []string{}
	for _, a := range actions {
		out = append(out, a.Key)
	}
	return out
}

func TestMalformedAndUnsupported(t *testing.T) {
	r := router()
	bad := signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "task.updated", "data": map[string]any{}})
	d := r.Decide(bad, route.State{}, replay.Ledger{})
	expect(t, d, route.Reject, route.ReasonMalformed)
	if d.Status != 400 {
		t.Errorf("status %d, want 400", d.Status)
	}
	other := signed(t, event.SourceVikunja, nil, map[string]any{"event_name": "project.updated", "data": map[string]any{"task": map[string]any{"id": 1}}})
	expect(t, r.Decide(other, route.State{}, replay.Ledger{}), route.Ignore, route.ReasonUnsupported)
	check := signed(t, event.SourceGitHub, map[string]string{event.GitHubEventHeader: "check_run", event.GitHubDeliveryHeader: "g"},
		map[string]any{"action": "completed", "repository": map[string]any{"full_name": "sperano/vigie"}})
	expect(t, r.Decide(check, route.State{}, replay.Ledger{}), route.Ignore, route.ReasonNotRouted)
}

func TestShadowOwnershipRecordsWithoutExecuting(t *testing.T) {
	r := router()
	r.Ownership = rollout.Table{Rules: []rollout.Rule{{Project: rollout.AllProjects, Family: run.VikunjaImplement,
		Owner: rollout.Shadow, Evidence: []rollout.Gate{rollout.ReplayParity}}}}
	d := r.Decide(updated(t, task(5, 136, nil, "claude-opus")), route.State{Task: fresh(5, 136, nil, "claude-opus")}, replay.Ledger{})
	fd := family(t, d)
	if fd.Owner != rollout.Shadow || fd.Execute || fd.Outcome != route.Dispatch {
		t.Errorf("shadow family %+v", fd)
	}
}

func TestDecideAuthenticatedSkipsOnlyTheSignature(t *testing.T) {
	r := route.Router{Config: profile.Defaults()}
	unsigned := event.Delivery{Source: event.SourceVikunja,
		Body: []byte(`{"event_name":"task.comment.created","data":{"task":{"id":1,"project_id":3},"comment":{"id":2,"comment":"hello"}}}`)}
	if d := r.Decide(unsigned, route.State{}, replay.Ledger{}); d.Outcome != route.Reject || d.Reason != "signature-missing" {
		t.Fatalf("Decide unsigned = %s/%s, want reject/signature-missing", d.Outcome, d.Reason)
	}
	d := r.DecideAuthenticated(unsigned, route.State{}, replay.Ledger{})
	if d.Outcome != route.Ignore || d.Reason != route.ReasonNotARequest || d.DeliveryKey != "vikunja/comment/2" {
		t.Fatalf("DecideAuthenticated = %+v, want the routed ignore/not-a-request", d)
	}
	other := event.Delivery{Source: "gitlab", Body: []byte(`{}`)}
	if d := r.DecideAuthenticated(other, route.State{}, replay.Ledger{}); d.Outcome != route.Ignore || d.Reason != route.ReasonUnsupported {
		t.Fatalf("DecideAuthenticated on an unknown source = %s/%s, want ignore/unsupported", d.Outcome, d.Reason)
	}
}

func TestOverallMatchesPrecedence(t *testing.T) {
	fams := []route.FamilyDecision{{Outcome: route.Duplicate}, {Outcome: route.Act}, {Outcome: route.Stale}}
	if got := route.Overall(fams); got != route.Act {
		t.Fatalf("Overall = %s, want act", got)
	}
	if got := route.Overall(nil); got != route.Ignore {
		t.Fatalf("Overall(nil) = %s, want ignore", got)
	}
}
