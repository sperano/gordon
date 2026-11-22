package kelos

import (
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The agent image's outputs at a subscription limit
// (docker/kelos-agent/limit_capture.py) and the agent's last answer.
const (
	ResultLimitResetsAt = "limit-resets-at"
	ResultLimitType     = "limit-type"
	ResultResponse      = "response"
)

// limitResponse and resetTime are limit_reaper.LIMIT_RESPONSE and
// RESET_TIME: Claude Code's "You've hit your ... limit" answer and its
// "resets 9:50am (UTC)".
var (
	limitResponse = regexp.MustCompile(`(?i)hit your .*limit`)
	resetTime     = regexp.MustCompile(`(?i)resets (\d{1,2})(?::(\d{2}))?\s*([ap]m)\s*\(UTC\)`)
)

// hoursPerHalfDay turns a 12-hour clock reading into a 24-hour one.
const (
	hoursPerHalfDay = 12
	minutesPerHour  = 60
)

// Response is the decoded `response` output (Kelos stores it base64), the
// raw value when it is not base64, "" without one.
func (t Task) Response() string {
	raw := t.result(ResultResponse)
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return raw
	}
	return string(b)
}

func (t Task) result(key string) string {
	if t.Status == nil {
		return ""
	}
	return t.Status.Results[key]
}

// LimitType is the limit that stopped the Task ("" when unknown).
func (t Task) LimitType() string { return t.result(ResultLimitType) }

// LimitStop reports a Task that failed on the agent's subscription limit
// (limit_reaper.is_limit_stop): the agent image's limit outputs, or else
// the agent's limit answer.
func (t Task) LimitStop() bool {
	if t.Phase() != PhaseFailed {
		return false
	}
	if _, ok := t.Status.Results[ResultLimitResetsAt]; ok {
		return true
	}
	if _, ok := t.Status.Results[ResultLimitType]; ok {
		return true
	}
	return limitResponse.MatchString(t.Response())
}

// LimitReset is when the limit that stopped the Task resets
// (limit_reaper.reset_time): the agent image's epoch, else the first
// "resets <h>[:<mm>]<am|pm> (UTC)" after the completion, else fallback
// after the completion. ok is false for a Task with no parsable
// completion time and no epoch: nothing tells when to restart it.
func (t Task) LimitReset(fallback time.Duration) (time.Time, bool) {
	if at := t.result(ResultLimitResetsAt); isDigits(at) {
		if s, err := strconv.ParseInt(at, 10, 64); err == nil {
			return time.Unix(s, 0).UTC(), true
		}
	}
	if t.Status == nil {
		return time.Time{}, false
	}
	done, err := time.Parse(time.RFC3339, t.Status.CompletionTime)
	if err != nil {
		return time.Time{}, false
	}
	done = done.UTC()
	if at, ok := resetFromText(t.Response(), done); ok {
		return at, true
	}
	return done.Add(fallback), true
}

// resetFromText is limit_reaper.reset_from_text.
func resetFromText(text string, completed time.Time) (time.Time, bool) {
	m := resetTime.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[1]) //nolint:errcheck // \d{1,2}
	hour %= hoursPerHalfDay
	if strings.EqualFold(m[3], "pm") {
		hour += hoursPerHalfDay
	}
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2]) //nolint:errcheck // \d{2}
	}
	if minute >= minutesPerHour {
		// Not a time (legacy's datetime.replace raised): the fallback.
		return time.Time{}, false
	}
	reset := time.Date(completed.Year(), completed.Month(), completed.Day(), hour, minute, 0, 0, time.UTC)
	if !reset.After(completed) {
		reset = reset.AddDate(0, 0, 1)
	}
	return reset, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
