package notice

import (
	"fmt"
	"strings"
)

// Kinds of the limit restart notices (notice_kind.RESTART, RESTART_GAVE_UP).
const (
	Restart       = "restart"
	RestartGaveUp = "restart-gave-up"
)

// Lucarne's run link sentence (wait_for_blockers.LUCARNE_RUN, LUCARNE_SIGN_IN).
const (
	lucarneRun    = "THE RUN IS ON LUCARNE"
	lucarneSignIn = "SIGN IN AND YOU LAND RIGHT ON IT."
)

// RestartInfo is what a limit restart's notice shows.
type RestartInfo struct {
	// Stopped is the Task the limit stopped, Restart the one restarting it.
	Stopped, Restart string
	// LimitType is the limit that stopped it ("" when unknown).
	LimitType string
	// Attempt is this restart's number, of at most Max.
	Attempt, Max int
	// RunURL is the restart's Lucarne run, "" for none.
	RunURL string
}

func limitHTML(kind string) string {
	if kind == "" {
		return ""
	}
	return " (" + code(kind) + ")"
}

// RestartText is limit_reaper.restarted_comment; its marker names the
// restart.
func RestartText(r RestartInfo) string {
	watch := ""
	if r.RunURL != "" {
		watch = fmt.Sprintf(`<p>WANT TO WATCH? <a href="%s">%s</a>. %s</p>`, escape(r.RunURL), lucarneRun, lucarneSignIn)
	}
	return fmt.Sprintf("<p>KELOS TASK %s STOPPED AT THE AGENT'S SUBSCRIPTION LIMIT%s. THE LIMIT HAS RESET. "+
		"TAKE ANOTHER LOOK, SONNY. IT'S GONNA HAPPEN AGAIN! BACK ON THE CASE AS %s (RESTART %d OF %d).</p>%s",
		code(r.Stopped), limitHTML(r.LimitType), code(r.Restart), r.Attempt, r.Max, watch) + Marker(Restart, r.Restart)
}

// GaveUpText is limit_reaper.gave_up_comment.
func GaveUpText(stopped, limitType string, maxRestarts int) string {
	return fmt.Sprintf("<p>KELOS TASK %s STOPPED AT THE AGENT'S SUBSCRIPTION LIMIT%s AGAIN AFTER %d RESTARTS. "+
		"AUTOMATIC RESTARTS STOP HERE. IT DOESN'T GET ANY BLUER. RETRY BY HAND: SEE %s, SECTION %s.</p>",
		code(stopped), limitHTML(limitType), maxRestarts, code("helm/kelos/README.md"), code("Subscription limits")) +
		Marker(RestartGaveUp, "")
}

// GaveUpPosted reports a gave-up notice about the stopped Task: its kind
// marker and the Task's name (the legacy marker names no Task).
func GaveUpPosted(text, stopped string) bool {
	return stopped != "" && HasKind(text, RestartGaveUp, "") && strings.Contains(text, code(stopped))
}
