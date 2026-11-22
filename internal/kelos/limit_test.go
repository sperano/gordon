package kelos

import (
	"encoding/base64"
	"testing"
	"time"
)

func failed(results map[string]string, completion string) Task {
	return Task{Status: &Status{Phase: PhaseFailed, Results: results, CompletionTime: completion}}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLimitStop(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want bool
	}{
		{"resets-at output", failed(map[string]string{ResultLimitResetsAt: "1"}, ""), true},
		{"limit-type output", failed(map[string]string{ResultLimitType: "weekly"}, ""), true},
		{"claude answer", failed(map[string]string{ResultResponse: b64("You've hit your session limit · resets 9:50am (UTC)")}, ""), true},
		{"plain answer", failed(map[string]string{ResultResponse: "You've HIT YOUR usage LIMIT"}, ""), true},
		{"other failure", failed(map[string]string{ResultResponse: b64("tests failed")}, ""), false},
		{"no results", failed(nil, ""), false},
		{"succeeded at a limit", Task{Status: &Status{Phase: PhaseSucceeded, Results: map[string]string{ResultLimitType: "x"}}}, false},
		{"no status", Task{}, false},
	}
	for _, c := range cases {
		if got := c.task.LimitStop(); got != c.want {
			t.Errorf("%s: LimitStop = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLimitReset(t *testing.T) {
	const done = "2026-11-20T10:00:00Z"
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name string
		task Task
		want time.Time
		ok   bool
	}{
		{"epoch output", failed(map[string]string{ResultLimitResetsAt: "1763640000"}, done), time.Unix(1763640000, 0).UTC(), true},
		{"later today", failed(map[string]string{ResultResponse: b64("resets 3pm (UTC)")}, done), at("2026-11-20T15:00:00Z"), true},
		{"earlier, so tomorrow", failed(map[string]string{ResultResponse: b64("resets 9:50am (UTC)")}, done), at("2026-11-21T09:50:00Z"), true},
		{"the completion minute itself is tomorrow", failed(map[string]string{ResultResponse: b64("resets 10am (UTC)")}, done), at("2026-11-21T10:00:00Z"), true},
		{"12am is midnight", failed(map[string]string{ResultResponse: b64("resets 12am (UTC)")}, done), at("2026-11-21T00:00:00Z"), true},
		{"12pm is noon", failed(map[string]string{ResultResponse: b64("RESETS 12:30PM (UTC)")}, done), at("2026-11-20T12:30:00Z"), true},
		{"bad minutes fall back", failed(map[string]string{ResultResponse: b64("resets 9:75am (UTC)")}, done), at("2026-11-20T11:00:00Z"), true},
		{"no reset in the answer", failed(map[string]string{ResultLimitType: "weekly"}, done), at("2026-11-20T11:00:00Z"), true},
		{"non-digit epoch falls back", failed(map[string]string{ResultLimitResetsAt: "soon"}, done), at("2026-11-20T11:00:00Z"), true},
		{"no completion time", failed(map[string]string{ResultLimitType: "weekly"}, ""), time.Time{}, false},
		{"no status", Task{}, time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := c.task.LimitReset(time.Hour)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: LimitReset = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestResponseAndLimitType(t *testing.T) {
	task := failed(map[string]string{ResultResponse: b64("done"), ResultLimitType: "five-hour"}, "")
	if got := task.Response(); got != "done" {
		t.Errorf("Response = %q", got)
	}
	if got := task.LimitType(); got != "five-hour" {
		t.Errorf("LimitType = %q", got)
	}
	if got := (Task{}).Response(); got != "" {
		t.Errorf("Response of a Task without status = %q", got)
	}
}
