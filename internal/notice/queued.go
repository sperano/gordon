package notice

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Queued is the kind of the notice on a Vikunja task whose run Gordon
// holds back from Kelos.
const Queued = "queued"

// Why Gordon holds a run back: its variant's concurrency limit is full,
// or another unfinished Task holds its branch in its Workspace.
const (
	HoldLimit  = "limit"
	HoldBranch = "branch"
)

// holdSep separates a hold detail's fields; no Task, variant or branch
// name contains it (git refuses ':' in a ref).
const holdSep = ":"

// Hold is why a run's submission is deferred.
type Hold struct {
	// Run is the held run's Task name.
	Run    string
	Reason string
	// Variant, Running and Limit describe a full variant limit.
	Variant        string
	Running, Limit int
	// Branch and Holder are a held branch and the Task holding it.
	Branch, Holder string
}

// ErrHold is a hold detail that names no hold.
var ErrHold = errors.New("bad hold detail")

// Key names the hold: the run, the reason and, for a full limit, the
// variant and its limit, the running count left out (a run held again
// for the same reason, however many Tasks its variant runs, is the same
// hold, and its notice is posted once); for a held branch, the branch
// and the Task holding it.
func (h Hold) Key() string {
	if h.Reason == HoldBranch {
		return strings.Join([]string{h.Run, HoldBranch, h.Branch, h.Holder}, holdSep)
	}
	return strings.Join([]string{h.Run, HoldLimit, h.Variant, strconv.Itoa(h.Limit)}, holdSep)
}

// Detail is the hold as a notice action's detail, read by ParseHold.
func (h Hold) Detail() string {
	if h.Reason == HoldBranch {
		return h.Key()
	}
	return h.Key() + holdSep + strconv.Itoa(h.Running)
}

// Message is the hold in a log line or a deferral reason.
func (h Hold) Message() string {
	if h.Reason == HoldBranch {
		return fmt.Sprintf("branch %s held by %s", h.Branch, h.Holder)
	}
	return fmt.Sprintf("variant %s runs %d of %d", h.Variant, h.Running, h.Limit)
}

// ParseHold reads a hold detail (Hold.Detail).
func ParseHold(detail string) (Hold, error) {
	f := strings.Split(detail, holdSep)
	bad := fmt.Errorf("%w: %q", ErrHold, detail)
	if len(f) != 4 && len(f) != 5 || f[0] == "" {
		return Hold{}, bad
	}
	switch h := (Hold{Run: f[0], Reason: f[1]}); {
	case h.Reason == HoldBranch && len(f) == 4 && f[2] != "" && f[3] != "":
		h.Branch, h.Holder = f[2], f[3]
		return h, nil
	case h.Reason == HoldLimit && len(f) == 5 && f[2] != "":
		var err1, err2 error
		h.Variant = f[2]
		h.Limit, err1 = strconv.Atoi(f[3])
		h.Running, err2 = strconv.Atoi(f[4])
		if err1 != nil || err2 != nil {
			return Hold{}, bad
		}
		return h, nil
	}
	return Hold{}, bad
}

// QueuedText is the notice of a held run: why it waits, and that it
// starts by itself.
func QueuedText(h Hold, label string) string {
	why := fmt.Sprintf("BRANCH %s IS HELD BY KELOS TASK %s", code(h.Branch), code(h.Holder))
	if h.Reason != HoldBranch {
		why = fmt.Sprintf("%s ALREADY RUNS %d OF ITS %d", code(h.Variant), h.Running, h.Limit)
	}
	return fmt.Sprintf("<p>HOLD YOUR HORSES. KELOS TASK %s WAITS IN LINE: %s. IT STARTS BY ITSELF "+
		"ONCE THAT CLEARS, AND THE %s LABEL COMES OFF.</p>", code(h.Run), why, code(label)) +
		Marker(Queued, h.Run)
}
