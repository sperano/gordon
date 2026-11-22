package route

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/run"
)

// Conflict routing reasons (reconcile_conflicts.py's skips).
const (
	ReasonWorkspaceNotConfigured = "workspace-not-configured"
	ReasonForeignHead            = "fork-or-missing-head-repository"
	ReasonEpisodeMoved           = "episode-moved"
	ReasonMergeabilityUnknown    = "mergeability-unknown"
	ReasonNoConflict             = "no-conflict"
	ReasonNoVariantLabel         = "no-kelos-label"
)

// Hash and SHA prefix lengths of a conflict episode
// (reconcile_conflicts.ROUTE_HASH_LENGTH, SHA_PREFIX_LENGTH).
const (
	routeHashLength = 8
	shaPrefixLength = 8
)

// Episode is reconcile_conflicts.conflict_payload's id: the route (the
// repository, Workspace and variant), the pull request and its base and
// head SHAs, so a repeated observation is the same run while either side
// moving is a new one.
func Episode(repo, workspace, variant string, number int, baseSHA, headSHA string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{repo, workspace, variant}, "\x00")))
	return fmt.Sprintf("%s-%d-%s-%s", hex.EncodeToString(sum[:])[:routeHashLength], number,
		prefix(baseSHA, shaPrefixLength), prefix(headSHA, shaPrefixLength))
}

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// conflict dispatches the run that resolves a confirmed merge conflict
// (conflictspawners.yaml): the conflict reconciliation's synthetic
// delivery names the pull request, its base and head, and the Workspace
// whose base it stacks on; the fresh pull request must still be the same
// open agent PR, in conflict on the same base and head SHAs. GitHub
// still computing mergeability is a retry.
func (r Router) conflict(ev event.Event, st State, led Ledger) Decision {
	gh := ev.GitHub
	if !gh.Synthetic {
		return Decision{Outcome: Ignore, Reason: ReasonActionNotRouted}
	}
	proj, ok := r.Config.WorkspaceProject(gh.Repository, gh.ConflictWorkspace)
	if !ok {
		return Decision{Outcome: Ignore, Reason: ReasonWorkspaceNotConfigured}
	}
	if st.PullRequest == nil {
		return Decision{Outcome: Retry, Reason: ReasonStateUnavailable}
	}
	fd := r.family(run.PRConflict, proj.Name)
	pr := *st.PullRequest
	if !r.conflictGate(&fd, gh, pr) {
		return Decision{Families: []FamilyDecision{fd}}
	}
	variant, ok := r.conflictVariant(&fd, pr)
	if !ok {
		return Decision{Families: []FamilyDecision{fd}}
	}
	id := run.Identity{Family: run.PRConflict, Project: proj.Name, Workspace: proj.Workspace, Variant: variant,
		PR: pr.Number, Episode: Episode(gh.Repository, proj.Workspace, variant, pr.Number, pr.BaseSHA, pr.HeadSHA),
		HeadRef: pr.HeadRef}
	req := profile.Request{Project: proj.Name, Variant: variant, Mode: profile.ModeFollowUp}
	if rn, submit, ok := r.submit(&fd, led, id, req); ok {
		target := action.PullRequest(gh.Repository, pr.Number)
		fd.Actions = append(fd.Actions, action.Notice(target, notice.Start, rn.Name).WaitFor(submit))
	}
	return Decision{Families: []FamilyDecision{fd}}
}

// conflictGate checks the fresh pull request against the episode sent
// (reconcile_conflicts.eligibility_problem, after the refresh).
func (r Router) conflictGate(fd *FamilyDecision, gh *event.GitHub, pr event.PullRequest) bool {
	sent := gh.PullRequest
	switch {
	case !slices.Contains(r.Config.Policy.PullRequestAuthors, pr.Author):
		fd.Outcome, fd.Reason = Reject, ReasonAuthorNotAllowed
	case pr.HeadRepo != gh.Repository:
		fd.Outcome, fd.Reason = Ignore, ReasonForeignHead
	case pr.State != event.PullRequestOpen:
		fd.Outcome, fd.Reason = Stale, ReasonPullRequestClosed
	case pr.Draft:
		fd.Outcome, fd.Reason = Stale, ReasonDraft
	case pr.HeadSHA != sent.HeadSHA || pr.BaseSHA != sent.BaseSHA || pr.BaseRef != sent.BaseRef:
		fd.Outcome, fd.Reason = Stale, ReasonEpisodeMoved
	case pr.Mergeable == nil:
		fd.Outcome, fd.Reason = Retry, ReasonMergeabilityUnknown
	case !pr.Conflicting():
		fd.Outcome, fd.Reason = Stale, ReasonNoConflict
	default:
		return true
	}
	return false
}

// conflictVariant is reconcile_conflicts.route_variant: a PR without a
// `kelos:` label is no agent's to fix; one with two non-default variant
// labels is a conflict.
func (r Router) conflictVariant(fd *FamilyDecision, pr event.PullRequest) (string, bool) {
	if !r.hasVariantLabel(pr) {
		fd.Outcome, fd.Reason = Ignore, ReasonNoVariantLabel
		return "", false
	}
	variant, ok := r.prVariant(pr)
	if !ok {
		fd.Outcome, fd.Reason = Conflict, ReasonMultipleVariantLabels
	}
	return variant, ok
}

func (r Router) hasVariantLabel(pr event.PullRequest) bool {
	prefix := r.Config.Policy.VariantLabelPrefix
	return slices.ContainsFunc(pr.Labels, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// ConflictVariant is the variant a conflict run of the pull request
// takes, false when it would take none (no `kelos:` label, or two
// variant labels), as the router decides it.
func (r Router) ConflictVariant(pr event.PullRequest) (string, bool) {
	if !r.hasVariantLabel(pr) {
		return "", false
	}
	return r.prVariant(pr)
}
