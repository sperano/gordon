package ghlife

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/githubapp"
	"github.com/sperano/gordon/internal/notice"
	"github.com/sperano/gordon/internal/worker"
)

// refusedStatuses are GitHub's answers to a merge it declines
// (auto_merge.REFUSED_STATUSES): a permission the token lacks (a workflow
// file needs Workflows), not mergeable (branch protection, a conflict, a
// moved base), the head changed since the verdict, a bad request.
var refusedStatuses = []int{http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusConflict,
	http.StatusUnprocessableEntity}

// pullState is what the merge reads of its pull request.
type pullState struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (e *Executor) pullState(ctx context.Context, t Target) (pullState, error) {
	var p pullState
	err := e.W.REST(ctx, t.Repo, pullsRead, http.MethodGet, path("repos", t.Owner, t.Name, "pulls", fmt.Sprint(t.N)), nil, &p)
	return p, err
}

// mergeApplied reports nothing left to merge: the pull request merged
// (by this merge or anyone), closed, or on another head than the one the
// verdict pinned (a new head gets its own verdict; legacy's pinned merge
// was refused and reported instead).
func (e *Executor) mergeApplied(ctx context.Context, a action.Action, t Target) (bool, error) {
	if err := pullTarget(a, t); err != nil {
		return false, err
	}
	if !sha.MatchString(a.Value) {
		return false, worker.Permanent(fmt.Errorf("merge head %q", a.Value))
	}
	p, err := e.pullState(ctx, t)
	if err != nil {
		return false, err
	}
	return p.Merged || p.State != "open" || p.Head.SHA != a.Value, nil
}

// merge is auto_merge.merge: GitHub's merge API with the head SHA pinned,
// so a push after the verdict makes GitHub refuse instead of merging an
// unchecked head; branch protection still applies. A merge GitHub
// declines is reported on the task once per head (AUTO-MERGE REFUSED)
// unless the pull request merged, closed or moved to another head
// meanwhile; the next scan tries again.
func (e *Executor) merge(ctx context.Context, a action.Action, t Target) error {
	p := path("repos", t.Owner, t.Name, "pulls", fmt.Sprint(t.N), "merge")
	body := map[string]string{"sha": a.Value, "merge_method": e.Policy.Readiness.Method()}
	err := e.W.REST(ctx, t.Repo, githubapp.Merge, http.MethodPut, p, body, nil)
	var st *ErrStatus
	if err == nil || !errors.As(err, &st) || !slices.Contains(refusedStatuses, st.Status) || !worker.IsPermanent(err) {
		return err
	}
	if done, err := e.mergeApplied(ctx, a, t); err != nil || done {
		return err
	}
	return e.reportRefusal(ctx, a, t, e.refusal(st))
}

// refusal is GitHub's answer as auto_merge's RequestError names it:
// method, URL, status and the start of the body.
func (e *Executor) refusal(st *ErrStatus) string {
	msg := fmt.Sprintf("%s %s%s: HTTP %d %s", st.Method, e.W.BaseURL, st.Path, st.Status, http.StatusText(st.Status))
	if st.Body != "" {
		msg += ": " + st.Body
	}
	return msg
}

// reportRefusal posts the AUTO-MERGE REFUSED notice on the task the
// action's Detail names, through the Vikunja executor (found again by its
// marker, once per head by its signature).
func (e *Executor) reportRefusal(ctx context.Context, a action.Action, t Target, msg string) error {
	task, err := strconv.ParseInt(a.Detail, 10, 64)
	if err != nil || task <= 0 {
		return worker.Permanent(fmt.Errorf("merge refusal task %q", a.Detail))
	}
	if e.Vikunja == nil {
		return worker.Permanent(errors.New("merge refused: no Vikunja writer to report it"))
	}
	h := notice.Head{Repo: t.FullName(), Number: int(t.N), SHA: a.Value, Extra: msg}
	return e.Vikunja.Execute(ctx, action.Notice(action.VikunjaTask(task), notice.MergeRefused, h.Detail()))
}
