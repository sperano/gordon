package restart

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
	"github.com/sperano/gordon/internal/store"
	"github.com/sperano/gordon/internal/submit"
)

// A restart's annotations (limit_reaper.RESTARTS, RESTART_OF): its number
// in the chain, and the Task it restarts, which the review supersede reads
// (a restart supersedes nothing).
const (
	RestartsAnnotation  = "kelos.spe.quebec/limit-restarts"
	RestartOfAnnotation = submit.RestartOf
)

// inboxAnnotation is the generation marker the restart keeps: the
// submission tells its own Task by it.
const inboxAnnotation = dispatch.InboxAnnotation

// copiedAnnotations are the annotation prefixes a restart keeps: the
// spawners' correlation annotations (limit_reaper.CORRELATION_PREFIX) and
// Gordon's generation and configuration markers; the restart marks of the
// stopped Task are replaced.
var copiedAnnotations = []string{"vikunja.spe.quebec/", "gordon.spe.quebec/"}

// Task is limit_reaper.restart_body: the stopped Task's spec and labels
// under the restart's name, its correlation and Gordon annotations, and
// the restart's own marks.
func Task(t kelos.Task, name string, attempt int) kelos.Task {
	ann := map[string]string{}
	for k, v := range t.Metadata.Annotations {
		for _, p := range copiedAnnotations {
			if strings.HasPrefix(k, p) {
				ann[k] = v
			}
		}
	}
	ann[RestartsAnnotation] = strconv.Itoa(attempt)
	ann[RestartOfAnnotation] = t.Metadata.Name
	return kelos.Task{
		Metadata: kelos.Metadata{Name: name, Labels: maps.Clone(t.Metadata.Labels), Annotations: ann},
		Spec:     t.Spec,
	}
}

// prFamilies are the runs announced on their pull request.
var prFamilies = []run.Family{run.PRFollowUp, run.PRConflict}

// restartWrites are the restart's submission and, once it is submitted,
// its notices: on a Vikunja task the restart notice and the start notice
// the restarted pod's init container posted in legacy; on a pull request
// the start notice (a review is announced by neither).
func (r *Reaper) restartWrites(f store.FailedRun, rn route.Run) []action.Action {
	submit := action.New(action.SubmitTask, action.KelosTask(route.KelosNamespace, rn.Name), "")
	out := []action.Action{submit}
	id := f.Identity
	if id.TaskID > 0 {
		target := action.VikunjaTask(id.TaskID)
		out = append(out, action.Notice(target, notice.Restart, rn.Name).WaitFor(submit),
			action.Notice(target, notice.Start, rn.Name).WaitFor(submit))
	}
	if target, ok := r.pullRequest(f); ok {
		out = append(out, action.Notice(target, notice.Start, rn.Name).WaitFor(submit))
	}
	return out
}

// gaveUp is the gave-up notice on the stopped run's Vikunja task and, for
// a run announced on its pull request, on that pull request (legacy
// reported a PR run's give-up nowhere); none for a run of neither.
func (r *Reaper) gaveUp(f store.FailedRun) []action.Action {
	var out []action.Action
	if f.Identity.TaskID > 0 {
		out = append(out, action.Notice(action.VikunjaTask(f.Identity.TaskID), notice.RestartGaveUp, f.Name))
	}
	if target, ok := r.pullRequest(f); ok {
		out = append(out, action.Notice(target, notice.RestartGaveUp, f.Name))
	}
	return out
}

// pullRequest is the pull request a run of prFamilies is announced on.
func (r *Reaper) pullRequest(f store.FailedRun) (action.Target, bool) {
	id := f.Identity
	repo := r.repository(f.Project)
	if repo == "" || id.PR <= 0 || !slices.Contains(prFamilies, id.Family) {
		return "", false
	}
	return action.PullRequest(repo, id.PR), true
}

func (r *Reaper) repository(project string) string {
	for _, p := range r.Config.Projects {
		if p.Name == project {
			return p.Repository
		}
	}
	return ""
}
