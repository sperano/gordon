// Package run defines run identities: which dispatch family a Kelos Task
// belongs to and the deterministic Task name and branch it gets.
//
// Follow-up, PR follow-up, review and conflict names are the legacy
// TaskSpawner nameTemplates (hollingsworth helm/kelos/templates/
// commentspawners.yaml, followupspawners.yaml, reviewspawners.yaml),
// because pod hooks (followup_queued_notice.py, limit_reaper.py) match
// them. An implementation or discuss run's name departs from the legacy
// taskspawners.yaml template: it leads with the Vikunja task id and
// carries the variant without its vendor prefix
// (vikunja-<task>-<project>-<short variant>, ShortVariant; a discuss run
// adds -discuss), so runs of one task sort together.
package run

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Family is a dispatch family: one legacy spawner family, or one group of
// model-free actions that Gordon takes over.
type Family string

const (
	VikunjaImplement       Family = "vikunja-implement"
	VikunjaDiscuss         Family = "vikunja-discuss"
	VikunjaFollowUp        Family = "vikunja-followup"
	VikunjaFollowUpAnswer  Family = "vikunja-followup-answer"
	VikunjaFollowUpDiscuss Family = "vikunja-followup-discuss"
	PRFollowUp             Family = "pr-followup"
	PRReview               Family = "pr-review"
	PROpen                 Family = "pr-open"
	MergeClose             Family = "merge-close"
	// PRConflict resolves a confirmed merge conflict of an agent pull
	// request, one run per base and head pair (conflictspawners.yaml).
	PRConflict Family = "pr-conflict"
	// The Vikunja lifecycle families (Gordon 06): model-free writes, each
	// the successor of one legacy hook or reconciler.
	VikunjaDragCancel  Family = "vikunja-drag-cancel"
	VikunjaDoneCleanup Family = "vikunja-done-cleanup"
	VikunjaChain       Family = "vikunja-chain"
	VikunjaFailure     Family = "vikunja-failure"
	// VikunjaAutoAssign assigns opted-in projects' new unassigned tasks.
	VikunjaAutoAssign Family = "vikunja-auto-assign"
	// LimitRestart restarts Gordon's runs a subscription limit stopped
	// (limit_reaper.py), once the limit reset.
	LimitRestart Family = "limit-restart"
	// PRReadiness announces an agent pull request's head ready to merge
	// or at its review round limit, labels its task, notifies and merges
	// on request (reconcile_pr_ready.py): model-free writes.
	PRReadiness Family = "pr-readiness"
	// VikunjaCost posts each finished run's completion cost notice on its
	// Vikunja task, from Sillage's estimate, and keeps the task total of
	// the latest one current (Gordon 12): model-free writes with no
	// legacy implementation.
	VikunjaCost Family = "vikunja-cost"
)

// Families lists every family the router decides, in a stable order.
var Families = []Family{
	VikunjaImplement, VikunjaDiscuss, VikunjaFollowUp, VikunjaFollowUpAnswer,
	VikunjaFollowUpDiscuss, PRFollowUp, PRReview, PROpen, MergeClose,
	VikunjaDragCancel, VikunjaDoneCleanup, VikunjaChain, VikunjaFailure, VikunjaAutoAssign,
	PRConflict, LimitRestart, PRReadiness, VikunjaCost,
}

// AgentFamilies are the families whose runs work on a Vikunja task as its
// agent (the implementation and discuss runs a drag to the backlog stops).
var AgentFamilies = []Family{VikunjaImplement, VikunjaDiscuss}

// BranchPrefix is the legacy agent branch prefix (vikunjaSpawners.branchPrefix).
const BranchPrefix = "kelos/vikunja-"

// MaxNameLength is the Kubernetes label-value limit a Task name must fit:
// Kelos copies the name into the kelos.dev/task label.
const MaxNameLength = 63

// shortSHALength is how much of a head SHA a review Task name keeps.
const shortSHALength = 8

// Identity is everything that names one run.
type Identity struct {
	Family    Family `json:"family"`
	Project   string `json:"project,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Variant   string `json:"variant,omitempty"`
	Reviewer  string `json:"reviewer,omitempty"`
	TaskID    int64  `json:"task_id,omitempty"`
	CommentID int64  `json:"comment_id,omitempty"`
	// CommentKind is the follow-up letter: c, l, r (GitHub) or p (poller).
	CommentKind string `json:"comment_kind,omitempty"`
	PR          int    `json:"pr,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	HeadRef     string `json:"head_ref,omitempty"`
	// Episode is a conflict's base and head pair
	// (reconcile_conflicts.conflict_payload's id).
	Episode string `json:"episode,omitempty"`
	// Retry is the limit restart this run is (limit_reaper.restart_name),
	// 0 for the original run.
	Retry int `json:"retry,omitempty"`
	// Ops marks a VikunjaImplement run in the ops mode (the legacy
	// vikunja-ops-* spawners): the task's implementation run, named
	// OpsNamePrefix-<task>-<project>-<short variant>.
	Ops bool `json:"ops,omitempty"`
}

// OpsNamePrefix starts an ops run's Task name (the legacy vikunja-ops-*
// spawners' nameTemplate).
const OpsNamePrefix = "vikunja-ops"

// discussNameSuffix ends a discuss run's Task name (before any -retry<n>),
// so it sorts with the task's implementation runs.
const discussNameSuffix = "discuss"

// vendorSeparator ends a variant's vendor prefix (claude-opus, gpt-sol).
const vendorSeparator = "-"

// ShortVariant is a variant as an implementation or discuss run's name
// carries it: without its vendor prefix (claude-opus is opus, gpt-sol is
// sol); a variant with no prefix stays whole. Distinct variants must keep
// distinct short names (dispatch.Config.Validate refuses a collision), or
// their runs of one task would share a name.
func ShortVariant(variant string) string {
	if _, rest, ok := strings.Cut(variant, vendorSeparator); ok && rest != "" {
		return rest
	}
	return variant
}

// Name is the Kelos Task name, or "" for a family that submits no Task;
// a limit restart's is its original's with -retry<n>.
func (i Identity) Name() string {
	name := i.baseName()
	if name != "" && i.Retry > 0 {
		name += fmt.Sprintf("-retry%d", i.Retry)
	}
	return name
}

func (i Identity) baseName() string {
	switch i.Family {
	case VikunjaImplement:
		if i.Ops {
			return fmt.Sprintf("%s-%d-%s-%s", OpsNamePrefix, i.TaskID, i.Project, ShortVariant(i.Variant))
		}
		return fmt.Sprintf("vikunja-%d-%s-%s", i.TaskID, i.Project, ShortVariant(i.Variant))
	case VikunjaDiscuss:
		return fmt.Sprintf("vikunja-%d-%s-%s-%s", i.TaskID, i.Project, ShortVariant(i.Variant), discussNameSuffix)
	case VikunjaFollowUp, VikunjaFollowUpAnswer, VikunjaFollowUpDiscuss:
		// One name across the three modes, so a task whose labels change
		// between deliveries still deduplicates (commentspawners.yaml).
		return fmt.Sprintf("vikunja-fu-%s-%d-c%d", i.Project, i.TaskID, i.CommentID)
	case PRFollowUp:
		return fmt.Sprintf("pr-followup-%s-%d-%s%d", i.Workspace, i.PR, i.CommentKind, i.CommentID)
	case PRReview:
		return fmt.Sprintf("review-%s-%s-%d-%s", i.Workspace, i.Reviewer, i.PR, short(i.HeadSHA))
	case PRConflict:
		return "pr-conflict-" + i.Episode
	default:
		return ""
	}
}

// Branch is the Task's spec.branch, which Kelos serialises Tasks on; ""
// for a Task that clones the Workspace ref (answers, discuss follow-ups,
// reviews).
func (i Identity) Branch() string {
	switch i.Family {
	case VikunjaImplement, VikunjaDiscuss, VikunjaFollowUp:
		return BranchPrefix + strconv.FormatInt(i.TaskID, 10)
	case PRFollowUp, PRConflict:
		return i.HeadRef
	default:
		return ""
	}
}

func short(sha string) string {
	if len(sha) < shortSHALength {
		return sha
	}
	return sha[:shortSHALength]
}

var agentBranch = regexp.MustCompile(`^` + regexp.QuoteMeta(BranchPrefix) + `([0-9]+)$`)

// TaskIDFromBranch returns the Vikunja task id of an agent branch
// (kelos/vikunja-<digits>, merge_close.task_id_from_branch); false for any
// other branch.
func TaskIDFromBranch(branch string) (int64, bool) {
	m := agentBranch.FindStringSubmatch(branch)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

var retrySuffix = regexp.MustCompile(`-retry[0-9]+$`)

// StripRetry removes the limit reaper's -retry<n> suffix from a Task name
// (limit_reaper.py), giving the name of the run it restarted.
func StripRetry(name string) string { return retrySuffix.ReplaceAllString(name, "") }
