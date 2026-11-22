package route

import (
	"errors"
	"regexp"
	"slices"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/run"
)

// KelosNamespace is where agent Tasks run.
const KelosNamespace = "kelos-agents"

// Reasons shared by several families.
const (
	ReasonRedelivery       = "redelivery"
	ReasonRequestHandled   = "request-handled"
	ReasonRunActive        = "run-active"
	ReasonRunExists        = "run-exists"
	ReasonStateUnavailable = "state-unavailable"
	ReasonNotARequest      = "not-a-request"
	ReasonNotOwner         = "not-owner"
	ReasonNotRouted        = "family-not-routed"
	ReasonRetired          = "family-retired"
	ReasonKelosOwned       = "kelos-owned"
	ReasonMalformed        = "malformed"
	ReasonUnsupported      = "unsupported-event"
	// ReasonNameTooLong refuses a run whose Task name exceeds
	// run.MaxNameLength: Kelos could not label the Task with it.
	ReasonNameTooLong = "task-name-too-long"
)

// Router decides deliveries. It holds only trusted configuration.
type Router struct {
	Config    profile.Config
	Ownership rollout.Table
	Keys      auth.Keys
}

// Decide authenticates, parses and routes one delivery.
func (r Router) Decide(d event.Delivery, st State, led Ledger) Decision {
	if err := r.Keys.Verify(d); err != nil {
		return Decision{Outcome: Reject, Reason: authReason(err), Status: auth.Status(err)}
	}
	return r.DecideAuthenticated(d, st, led)
}

// DecideAuthenticated parses and routes a delivery whose signature the
// receiver already verified before persisting it, so the worker deciding
// it holds no webhook key.
func (r Router) DecideAuthenticated(d event.Delivery, st State, led Ledger) Decision {
	switch d.Source {
	case event.SourceWikiComment:
		return Decision{Outcome: Ignore, Reason: ReasonRetired}
	case event.SourceImageBuild:
		return Decision{Outcome: Ignore, Reason: ReasonKelosOwned}
	}
	ev, err := parse(d)
	if err != nil {
		return parseRefusal(err)
	}
	if led.SeenDelivery(ev.DeliveryKey) {
		return Decision{Outcome: Duplicate, Reason: ReasonRedelivery, DeliveryKey: ev.DeliveryKey, RequestKey: ev.RequestKey}
	}
	dec := r.route(ev, st, led)
	dec.DeliveryKey, dec.RequestKey = ev.DeliveryKey, ev.RequestKey
	if dec.Outcome == "" {
		dec.Outcome = overall(dec.Families)
	}
	return dec
}

func (r Router) route(ev event.Event, st State, led Ledger) Decision {
	switch ev.Kind {
	case event.VikunjaTaskCreated:
		return r.taskCreated(ev, st)
	case event.VikunjaTaskUpdated:
		return r.taskUpdated(ev, st, led)
	case event.VikunjaTaskCommentCreated:
		return r.taskComment(ev, st, led)
	case event.GitHubIssueComment, event.GitHubReview, event.GitHubReviewComment:
		return r.prComment(ev, st, led)
	case event.GitHubPullRequest:
		return r.pullRequest(ev, st, led)
	case event.GitHubCheckRun:
		return r.checkRun(ev, st, led)
	default:
		return Decision{Outcome: Ignore, Reason: ReasonNotRouted}
	}
}

func parse(d event.Delivery) (event.Event, error) { return event.Parse(d) }

func authReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrUnsigned):
		return "signature-missing"
	case errors.Is(err, auth.ErrBadSignature):
		return "signature-invalid"
	case errors.Is(err, auth.ErrTooLarge):
		return "body-too-large"
	default:
		return "unknown-source"
	}
}

func parseRefusal(err error) Decision {
	if errors.Is(err, event.ErrUnsupported) {
		return Decision{Outcome: Ignore, Reason: ReasonUnsupported}
	}
	return Decision{Outcome: Reject, Reason: ReasonMalformed, Status: auth.Status(err)}
}

// family starts a family decision with its ownership filled in.
func (r Router) family(f run.Family, project string) FamilyDecision {
	return FamilyDecision{
		Family: f, Project: project,
		Owner:   r.Ownership.Owner(project, f),
		Execute: r.Ownership.Executes(project, f),
	}
}

// resolved builds a run, or a Reject naming the configuration's refusal
// or a Task name too long for its kelos.dev/task label.
func (r Router) resolved(fd *FamilyDecision, id run.Identity, req profile.Request) (Run, bool) {
	if len(id.Name()) > run.MaxNameLength {
		fd.Outcome, fd.Reason = Reject, ReasonNameTooLong
		return Run{}, false
	}
	prof, err := r.Config.Resolve(req)
	if err != nil {
		var perr *profile.Error
		fd.Outcome, fd.Reason = Reject, err.Error()
		if errors.As(err, &perr) {
			fd.Reason = perr.Code
		}
		return Run{}, false
	}
	return Run{Name: id.Name(), Branch: id.Branch(), Identity: id, Profile: prof}, true
}

// existing classifies a run the ledger already has: active or still
// present in Kelos is a duplicate, as the legacy Task name was; a record
// whose Task is gone and finished lets a new request run again.
func existing(led Ledger, name string) (string, bool) {
	rec, ok := led.Run(name)
	switch {
	case !ok:
		return "", false
	case rec.Active:
		return ReasonRunActive, true
	case rec.Exists:
		return ReasonRunExists, true
	default:
		return "", false
	}
}

// isRequest matches an owner command the way the legacy filters do: the
// trigger at the start of the text (after an optional <p> in Vikunja's
// HTML), then whitespace, a tag or the end.
func isRequest(text, trigger string, html bool) bool {
	prefix := `^`
	if html {
		prefix = `^(<p>)?`
	}
	end := `(\s|$)`
	if html {
		end = `([ \t\n\f\r]|<|$)`
	}
	return regexp.MustCompile(prefix + regexp.QuoteMeta(trigger) + end).MatchString(text)
}

// IsVikunjaRequest reports whether a Vikunja comment's HTML is an owner
// command for trigger, as the router matches it.
func IsVikunjaRequest(text, trigger string) bool { return isRequest(text, trigger, true) }

// IsGitHubRequest reports whether a GitHub comment's or review's Markdown
// is an owner command for trigger, as the router matches it.
func IsGitHubRequest(text, trigger string) bool { return isRequest(text, trigger, false) }

// agents returns the task's assignees that are agent variants.
func (r Router) agents(t event.Task) []string {
	var out []string
	for _, a := range t.Assignees {
		if r.Config.IsAgent(a) && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}
