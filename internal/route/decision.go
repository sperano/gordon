// Package route turns an authenticated delivery, the fresh upstream state
// it concerns and the ledger of handled work into a Decision: which runs
// to submit and which writes to make, per dispatch family. Deciding has
// no side effects; the rollout table says whether Gordon executes a
// family's decision or only records it (shadow).
package route

import (
	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
)

// Outcome classifies a decision.
type Outcome string

const (
	// Dispatch submits at least one run.
	Dispatch Outcome = "dispatch"
	// Act makes writes without a run (the model-free families).
	Act Outcome = "act"
	// Ignore: the event asks for nothing (not a request, not eligible).
	Ignore Outcome = "ignore"
	// Reject: the source or the actor is not authorised, or the
	// configuration refuses the resolved profile.
	Reject Outcome = "reject"
	// Duplicate: a redelivery, or a request or run already handled.
	Duplicate Outcome = "duplicate"
	// Stale: the event qualified when sent, the fresh state no longer does.
	Stale Outcome = "stale"
	// Conflict: the state selects more than one agent or variant.
	Conflict Outcome = "conflict"
	// Retry: the fresh state is unavailable; decide again later.
	Retry Outcome = "retry"
)

// precedence orders family outcomes for the delivery's overall outcome.
var precedence = []Outcome{Dispatch, Act, Conflict, Stale, Reject, Retry, Duplicate, Ignore}

// Decision is the routing decision for one delivery.
type Decision struct {
	Outcome     Outcome          `json:"outcome"`
	Reason      string           `json:"reason,omitempty"`
	Status      int              `json:"status,omitempty"`
	DeliveryKey string           `json:"delivery_key,omitempty"`
	RequestKey  string           `json:"request_key,omitempty"`
	Families    []FamilyDecision `json:"families,omitempty"`
}

// FamilyDecision is the decision of one dispatch family.
type FamilyDecision struct {
	Family  run.Family      `json:"family"`
	Project string          `json:"project,omitempty"`
	Owner   rollout.Owner   `json:"owner"`
	Execute bool            `json:"execute"`
	Outcome Outcome         `json:"outcome"`
	Reason  string          `json:"reason,omitempty"`
	Runs    []Run           `json:"runs,omitempty"`
	Actions []action.Action `json:"actions,omitempty"`
}

// Run is one Kelos Task to submit.
type Run struct {
	Name     string           `json:"name"`
	Branch   string           `json:"branch,omitempty"`
	Identity run.Identity     `json:"identity"`
	Profile  profile.Resolved `json:"profile"`
}

// State is the fresh upstream state a decision reads: Gordon re-reads the
// task or pull request before deciding instead of trusting the payload.
type State struct {
	Task        *event.Task        `json:"task,omitempty"`
	PullRequest *event.PullRequest `json:"pull_request,omitempty"`
}

// RunRecord is the ledger's record of a run: Active while its Kelos Task
// is pending or running; Exists while the Kelos Task object is present
// (the ledger keeps the record after Kelos cleans the Task up).
type RunRecord struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Exists bool   `json:"exists"`
}

// Ledger is the durable record of handled deliveries, requests and runs.
type Ledger interface {
	SeenDelivery(key string) bool
	HandledRequest(key string) bool
	Run(name string) (RunRecord, bool)
}

// overall is the strongest family outcome, by precedence.
func overall(families []FamilyDecision) Outcome {
	for _, o := range precedence {
		for _, f := range families {
			if f.Outcome == o {
				return o
			}
		}
	}
	return Ignore
}
