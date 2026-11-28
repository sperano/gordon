package dispatch

import (
	"strconv"
	"strings"

	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// Input is everything one run's Task is built from: the run as routed
// (identity and resolved profile), the delivery and the fresh state the
// router decided on, the inbox row that recorded it, and the task's
// comments ("" when not fetched or the fetch failed).
type Input struct {
	Run      route.Run
	Event    event.Event
	State    route.State
	InboxID  int64
	Comments string
}

// Data is the template data of a run, a map so that templates read the
// legacy Kelos variable names:
//
//   - every run: name, family, project, project_title, workspace,
//     repository, variant, short_variant (run.ShortVariant: the variant
//     without its vendor prefix, as in an implementation or discuss run's
//     name), reviewer, profile, mode, model, effort, branch (the Task's
//     spec.branch);
//   - Vikunja families: task, comment, Title (the fresh task title), Body
//     (the description for implement and discuss, the request comment
//     for follow-ups), description, author, Context.comments, Branch;
//   - GitHub families: Number, Title, URL, Branch (the PR head), Event
//     (issue_comment, pull_request_review_comment, pull_request_review,
//     pull_request), Action, CommentURL, CommentBody and
//     Payload.Comment.ID, Payload.Comment.PullRequestReviewID,
//     Payload.Review.ID, Payload.PullRequest.Head.SHA (and .GetSHA),
//     Payload.PullRequest.Head.Ref, Payload.PullRequest.Base.Ref;
//   - pr-conflict, the conflict dispatch's fields: id and ID (the
//     episode), number, title, url, base_ref, base_sha, head_sha;
//   - image-build-push, the catalogued image's fields instead of the
//     GitHub ones: image, environment, repository (the image's),
//     coalesce and owner_merged (bools), first_branch (its first branch,
//     the one a coalescing build follows) and Payload.After and Payload.GetAfter
//     (the pushed commit, as go-github's accessor the push spawners use).
type Data map[string]any

func (b Builder) data(in Input, proj profile.Project) (Data, error) {
	id, prof := in.Run.Identity, in.Run.Profile
	d := Data{
		"name": in.Run.Name, "family": string(id.Family), "project": proj.Name, "project_title": proj.Title,
		"workspace": proj.Workspace, "repository": proj.Repository, "variant": id.Variant,
		"short_variant": run.ShortVariant(id.Variant),
		"reviewer":      id.Reviewer, "profile": prof.Profile, "mode": string(prof.Mode), "model": prof.Model,
		"effort": prof.Effort, "branch": in.Run.Branch, "Branch": in.Run.Branch,
	}
	switch {
	case id.Family == run.ImageBuildPush:
		return d, b.pushData(d, id)
	case in.Event.Vikunja != nil:
		vikunjaData(d, in)
	case in.Event.GitHub != nil:
		githubData(d, in)
		if id.Family == run.PRConflict {
			conflictData(d, in)
		}
	}
	return d, nil
}

// pushData are an image build's fields, from its catalogue entry: the
// image named by the run, never by the payload.
func (b Builder) pushData(d Data, id run.Identity) error {
	img, ok := b.Routing.ImageBuilds.Image(id.Image)
	if !ok || len(img.Branches) == 0 {
		return refuse(CodeUnknownImage, "image %q", id.Image)
	}
	d["image"], d["environment"], d["repository"] = img.Name, img.Environment, img.Repository
	d["coalesce"], d["owner_merged"], d["first_branch"] = img.Coalesce, img.OwnerMerged, img.Branches[0]
	d["Payload"] = map[string]any{"After": id.HeadSHA, "GetAfter": id.HeadSHA}
	return nil
}

// conflictData are the conflict dispatch's fields
// (reconcile_conflicts.conflict_payload), from the fresh pull request.
func conflictData(d Data, in Input) {
	pr := in.Event.GitHub.PullRequest
	if in.State.PullRequest != nil {
		pr = *in.State.PullRequest
	}
	d["id"], d["ID"] = in.Run.Identity.Episode, in.Run.Identity.Episode
	d["number"], d["title"], d["url"] = strconv.Itoa(pr.Number), pr.Title, pr.URL
	d["base_ref"], d["base_sha"], d["head_sha"] = pr.BaseRef, pr.BaseSHA, pr.HeadSHA
}

func vikunjaData(d Data, in Input) {
	v, id := in.Event.Vikunja, in.Run.Identity
	// The fresh task is authoritative, an emptied description included;
	// the delivery's copy stands in only without one.
	task := v.Task
	if in.State.Task != nil {
		task = *in.State.Task
	}
	d["task"] = strconv.FormatInt(id.TaskID, 10)
	d["Title"], d["description"], d["Body"] = task.Title, task.Description, task.Description
	d["comment"], d["author"] = "", ""
	if v.Comment != nil {
		d["comment"] = strconv.FormatInt(v.Comment.ID, 10)
		d["author"], d["Body"] = in.Event.Actor, v.Comment.Body
	}
	d["Context"] = map[string]any{"comments": in.Comments}
}

func githubData(d Data, in Input) {
	gh := in.Event.GitHub
	pr := gh.PullRequest
	if in.State.PullRequest != nil {
		pr = *in.State.PullRequest
	}
	d["Number"] = strconv.Itoa(pr.Number)
	d["Title"], d["URL"], d["Branch"] = pr.Title, pr.URL, pr.HeadRef
	d["Event"] = strings.TrimPrefix(string(in.Event.Kind), "github/")
	d["Action"] = in.Event.Action
	d["CommentURL"], d["CommentBody"] = gh.CommentURL, gh.Body
	comment := map[string]any{"ID": "", "PullRequestReviewID": ""}
	review := map[string]any{"ID": ""}
	switch gh.CommentKind {
	case event.CommentKindIssue, event.CommentKindInline:
		comment["ID"] = strconv.FormatInt(gh.CommentID, 10)
		if gh.ReviewID > 0 {
			comment["PullRequestReviewID"] = strconv.FormatInt(gh.ReviewID, 10)
		}
	case event.CommentKindReview:
		review["ID"] = strconv.FormatInt(gh.CommentID, 10)
	}
	d["Payload"] = map[string]any{
		"Comment": comment, "Review": review,
		"PullRequest": map[string]any{
			// GetSHA is go-github's accessor, which the legacy review
			// Task name slices.
			"Head": map[string]any{"SHA": pr.HeadSHA, "GetSHA": pr.HeadSHA, "Ref": pr.HeadRef},
			"Base": map[string]any{"Ref": pr.BaseRef},
		},
	}
}

// vikunjaFamily reports a family whose Task quotes a Vikunja task.
func vikunjaFamily(f run.Family) bool {
	switch f {
	case run.VikunjaImplement, run.VikunjaDiscuss, run.VikunjaFollowUp, run.VikunjaFollowUpAnswer, run.VikunjaFollowUpDiscuss:
		return true
	}
	return false
}
