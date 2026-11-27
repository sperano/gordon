// Package run defines run identities: which dispatch family a Kelos Task
// belongs to and the deterministic Task name and branch it gets.
//
// Follow-up, PR follow-up and conflict names are the legacy TaskSpawner
// nameTemplates (hollingsworth helm/kelos/templates/commentspawners.yaml,
// followupspawners.yaml), because pod hooks (followup_queued_notice.py)
// match them. A review of an agent pull request (branch
// kelos/vikunja-<task>) leads with its task id,
// vikunja-<task>-<workspace>-<reviewer>-<pr>-<sha8>; any other review,
// and one that name would make too long, keeps the reviewspawners.yaml
// name, review-<workspace>-<reviewer>-<pr>-<sha8>. An implementation or
// discuss run's name departs from the legacy taskspawners.yaml template:
// it leads with the Vikunja task id and carries the variant without its
// vendor prefix (vikunja-<task>-<project>-<short variant>, ShortVariant;
// a discuss run adds -discuss), so runs of one task sort together. An
// image build for a push keeps the push spawner's name,
// image-build-<image>-push-<sha12>.
package run

import (
	"fmt"
	"regexp"
	"slices"
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
	// VikunjaQueued labels and explains a Vikunja task's run Gordon holds
	// back from Kelos, and takes the label off once no run of the task is
	// held: model-free writes with no legacy implementation.
	VikunjaQueued Family = "vikunja-queued"
	// ImageBuildPush builds a catalogued image for a push to one of its
	// branches: one model-free runner Task per image and pushed commit
	// (the image-build-<image>-push spawners of Hollingsworth's
	// image-builds.yaml).
	ImageBuildPush Family = "image-build-push"
)

// Families lists every family the router decides, in a stable order.
var Families = []Family{
	VikunjaImplement, VikunjaDiscuss, VikunjaFollowUp, VikunjaFollowUpAnswer,
	VikunjaFollowUpDiscuss, PRFollowUp, PRReview, PROpen, MergeClose,
	VikunjaDragCancel, VikunjaDoneCleanup, VikunjaChain, VikunjaFailure, VikunjaAutoAssign,
	PRConflict, LimitRestart, PRReadiness, VikunjaCost, VikunjaQueued, ImageBuildPush,
}

// AgentFamilies are the families whose runs work on a Vikunja task as its
// agent (the implementation and discuss runs a drag to the backlog stops).
var AgentFamilies = []Family{VikunjaImplement, VikunjaDiscuss}

// TaskFamilies are the families whose runs come from a Vikunja task: its
// agent runs and its follow-ups.
var TaskFamilies = append(slices.Clone(AgentFamilies), VikunjaFollowUp, VikunjaFollowUpAnswer, VikunjaFollowUpDiscuss)

// BranchPrefix is the legacy agent branch prefix (vikunjaSpawners.branchPrefix).
const BranchPrefix = "kelos/vikunja-"

// MaxNameLength is the Kubernetes label-value limit a Task name must fit:
// Kelos copies the name into the kelos.dev/task label.
const MaxNameLength = 63

// shortSHALength is how much of a head SHA a review Task name keeps;
// pushSHALength how much of a pushed commit an image build's keeps (the
// push spawners' printf "%.12s").
const (
	shortSHALength = 8
	pushSHALength  = 12
)

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
	// Image is an ImageBuildPush run's catalogued image; HeadSHA its
	// pushed commit.
	Image string `json:"image,omitempty"`
}

// OpsNamePrefix starts an ops run's Task name (the legacy vikunja-ops-*
// spawners' nameTemplate).
const OpsNamePrefix = "vikunja-ops"

// agentNamePrefix starts the name of a run tied to a Vikunja task by its
// id; reviewNamePrefix starts a review of a pull request with no task.
const (
	agentNamePrefix  = "vikunja"
	reviewNamePrefix = "review"
)

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
	suffix := ""
	if i.Retry > 0 {
		suffix = fmt.Sprintf("-retry%d", i.Retry)
	}
	name := i.baseName(suffix)
	if name == "" {
		return ""
	}
	return name + suffix
}

// baseName is the name before suffix, the -retry<n> a restart appends.
func (i Identity) baseName(suffix string) string {
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
		return i.reviewName(suffix)
	case PRConflict:
		return "pr-conflict-" + i.Episode
	case ImageBuildPush:
		return fmt.Sprintf("image-build-%s-push-%s", i.Image, PushRequest(i.HeadSHA))
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

// reviewName leads an agent pull request's review with its Vikunja task
// id, vikunja-<task>-<workspace>-<reviewer>-<pr>-<sha8>, so it sorts with
// the task's other runs. A review of any other branch, or one whose
// task-led name and suffix would not fit MaxNameLength, keeps
// review-<workspace>-<reviewer>-<pr>-<sha8>. The head SHA keeps each
// head's review its own name.
func (i Identity) reviewName(suffix string) string {
	rest := fmt.Sprintf("%s-%s-%d-%s", i.Workspace, i.Reviewer, i.PR, short(i.HeadSHA))
	if task, ok := TaskIDFromBranch(i.HeadRef); ok {
		name := fmt.Sprintf("%s-%d-%s", agentNamePrefix, task, rest)
		if len(name)+len(suffix) <= MaxNameLength {
			return name
		}
	}
	return reviewNamePrefix + "-" + rest
}

func short(sha string) string { return prefix(sha, shortSHALength) }

// PushRequest is the part of a pushed commit an image build's Task name
// and request id carry: its first 12 characters.
func PushRequest(sha string) string { return prefix(sha, pushSHALength) }

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
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
