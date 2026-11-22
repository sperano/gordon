package event

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestHeaderCaseInsensitive(t *testing.T) {
	d := Delivery{Headers: map[string]string{"x-github-event": "push"}}
	if got := d.Header("X-GitHub-Event"); got != "push" {
		t.Errorf("Header = %q, want push", got)
	}
	if got := d.Header("X-Missing"); got != "" {
		t.Errorf("missing header = %q, want empty", got)
	}
}

func TestParseVikunjaTaskUpdated(t *testing.T) {
	body := []byte(`{"event_name":"task.updated","data":{"task":{"id":7,"project_id":3,"done":false,` +
		`"labels":[{"title":"in progress"}],"assignees":[{"username":"claude-opus"}]}}}`)
	ev, err := ParseVikunja(body)
	if err != nil {
		t.Fatal(err)
	}
	// sha256 of the compacted data object, first 16 bytes, from Python hashlib.
	const key = "vikunja/task.updated/7/38934adbfad18e550274d38705aed57e"
	if ev.Kind != VikunjaTaskUpdated || ev.RequestKey != "vikunja/task/7" {
		t.Errorf("kind %q request %q", ev.Kind, ev.RequestKey)
	}
	if ev.DeliveryKey != key {
		t.Errorf("delivery key %q, want %q", ev.DeliveryKey, key)
	}
	task := ev.Vikunja.Task
	if task.ProjectID != 3 || !slices.Equal(task.Labels, []string{"in progress"}) ||
		!slices.Equal(task.Assignees, []string{"claude-opus"}) || task.Buckets != nil {
		t.Errorf("task %+v", task)
	}
}

func TestParseVikunjaTaskCreated(t *testing.T) {
	body := []byte(`{"event_name":"task.created","data":{"task":{"id":7,"project_id":3}}}`)
	ev, err := ParseVikunja(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != VikunjaTaskCreated || ev.DeliveryKey != "vikunja/task.created/7" ||
		ev.RequestKey != "vikunja/task.created/7" {
		t.Fatalf("created event %+v", ev)
	}
	changed, err := ParseVikunja([]byte(`{"event_name":"task.created","data":{"task":{"id":7,"project_id":9,"title":"changed"}}}`))
	if err != nil || changed.DeliveryKey != ev.DeliveryKey || changed.RequestKey != ev.RequestKey {
		t.Fatalf("changed snapshot keys %q/%q err %v", changed.DeliveryKey, changed.RequestKey, err)
	}
}

func TestParseVikunjaTaskTitleAndDescription(t *testing.T) {
	ev, err := ParseVikunja([]byte(`{"event_name":"task.updated","data":{"task":{"id":7,"project_id":3,` +
		`"title":"Ship it","description":"<p>now</p>"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if task := ev.Vikunja.Task; task.Title != "Ship it" || task.Description != "<p>now</p>" {
		t.Fatalf("task %+v", task)
	}
}

// TestParseVikunjaDigestIgnoresFormatting pins that a re-formatted resend
// of the same data gets the same delivery key: the digest is of the
// compacted data.
func TestParseVikunjaDigestIgnoresFormatting(t *testing.T) {
	pretty := []byte("{\"event_name\": \"task.updated\",\n  \"data\": {\n    \"task\": {\"id\": 7, \"project_id\": 3,\n" +
		"      \"done\": false,\n      \"labels\": [ {\"title\": \"in progress\"} ],\n" +
		"      \"assignees\": [ {\"username\": \"claude-opus\"} ]\n    }\n  }\n}")
	ev, err := ParseVikunja(pretty)
	if err != nil {
		t.Fatal(err)
	}
	// The same key as TestParseVikunjaTaskUpdated's compact body.
	const key = "vikunja/task.updated/7/38934adbfad18e550274d38705aed57e"
	if ev.DeliveryKey != key {
		t.Errorf("delivery key %q, want %q", ev.DeliveryKey, key)
	}
}

func TestParseVikunjaKeepsEmptyBuckets(t *testing.T) {
	ev, err := ParseVikunja([]byte(`{"event_name":"task.updated","data":{"task":{"id":1,"buckets":[]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Vikunja.Task.Buckets == nil || len(ev.Vikunja.Task.Buckets) != 0 {
		t.Errorf("buckets %#v, want empty non-nil (the enricher's 'in no bucket')", ev.Vikunja.Task.Buckets)
	}
}

func TestParseVikunjaCommentKeys(t *testing.T) {
	body := `{"event_name":"task.comment.created","data":{"task":{"id":5},"comment":{"id":9,"comment":"<p>/gordon x</p>"},` +
		`"doer":{"username":"eric"}%s}}`
	ev, err := ParseVikunja([]byte(fmt.Sprintf(body, "")))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Actor != "eric" || ev.Vikunja.Comment.Body != "<p>/gordon x</p>" {
		t.Errorf("actor %q comment %+v", ev.Actor, ev.Vikunja.Comment)
	}
	if ev.DeliveryKey != "vikunja/comment/9" || ev.RequestKey != "vikunja/comment/9" {
		t.Errorf("keys %q %q", ev.DeliveryKey, ev.RequestKey)
	}
	resend, err := ParseVikunja([]byte(fmt.Sprintf(body, `,"redispatch":"2026-11-19T08:00:00Z"`)))
	if err != nil {
		t.Fatal(err)
	}
	if resend.RequestKey != ev.RequestKey || resend.DeliveryKey != "vikunja/comment/9/redispatch/2026-11-19T08:00:00Z" {
		t.Errorf("resend keys %q %q", resend.DeliveryKey, resend.RequestKey)
	}
}

func TestParseVikunjaRefusals(t *testing.T) {
	cases := map[string]struct {
		body string
		want error
	}{
		"not json":      {`{`, ErrMalformed},
		"data not obj":  {`{"event_name":"task.updated","data":[1]}`, ErrMalformed},
		"no data":       {`{"event_name":"task.updated"}`, ErrMalformed},
		"no task id":    {`{"event_name":"task.updated","data":{"task":{}}}`, ErrMalformed},
		"zero task id":  {`{"event_name":"task.updated","data":{"task":{"id":0}}}`, ErrMalformed},
		"negative id":   {`{"event_name":"task.updated","data":{"task":{"id":-1}}}`, ErrMalformed},
		"project event": {`{"event_name":"project.updated","data":{"project":{"id":3}}}`, ErrUnsupported},
		"other no data": {`{"event_name":"task.deleted"}`, ErrUnsupported},
		"comment no id": {`{"event_name":"task.comment.created","data":{"task":{"id":1},"comment":{}}}`, ErrMalformed},
		"other event":   {`{"event_name":"task.deleted","data":{"task":{"id":1}}}`, ErrUnsupported},
	}
	for name, c := range cases {
		if _, err := ParseVikunja([]byte(c.body)); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
}

func github(event, guid, body string) Delivery {
	return Delivery{Source: SourceGitHub, Body: []byte(body),
		Headers: map[string]string{GitHubEventHeader: event, GitHubDeliveryHeader: guid}}
}

func TestParseGitHubIssueComment(t *testing.T) {
	ev, err := ParseGitHub(github("issue_comment", "g1", `{"action":"created","sender":{"login":"sperano"},`+
		`"repository":{"full_name":"sperano/vigie"},"issue":{"number":4},"comment":{"id":11,"body":"/gordon"}}`))
	if err != nil {
		t.Fatal(err)
	}
	gh := ev.GitHub
	if ev.DeliveryKey != "github/delivery/g1" || ev.RequestKey != "github/sperano/vigie/c/11" {
		t.Errorf("keys %q %q", ev.DeliveryKey, ev.RequestKey)
	}
	if gh.OnPullRequest || gh.CommentKind != CommentKindIssue || gh.PullRequest.Number != 4 || ev.Actor != "sperano" {
		t.Errorf("event %+v github %+v", ev, gh)
	}
}

func TestParseGitHubIssueCommentOnPullRequest(t *testing.T) {
	ev, err := ParseGitHub(github("issue_comment", "g5", `{"action":"created","sender":{"login":"sperano"},`+
		`"repository":{"full_name":"sperano/vigie"},"issue":{"number":6,"pull_request":{}},`+
		`"comment":{"id":12,"node_id":"IC_12","body":"/gordon","html_url":"https://github.com/sperano/vigie/pull/6#issuecomment-12"}}`))
	if err != nil {
		t.Fatal(err)
	}
	gh := ev.GitHub
	if !gh.OnPullRequest || gh.PullRequest.Number != 6 || gh.PullRequest.Repository != "sperano/vigie" {
		t.Errorf("github %+v, want on pull request sperano/vigie#6", gh)
	}
	if ev.RequestKey != "github/sperano/vigie/c/12" || gh.CommentNodeID != "IC_12" ||
		gh.CommentURL != "https://github.com/sperano/vigie/pull/6#issuecomment-12" || gh.ReviewID != 0 {
		t.Errorf("request key %q, node id %q, comment url %q, review id %d", ev.RequestKey, gh.CommentNodeID,
			gh.CommentURL, gh.ReviewID)
	}
}

func TestParseGitHubReviewAndPullRequest(t *testing.T) {
	pr := `"pull_request":{"number":3,"title":"Fix","html_url":"https://github.com/sperano/vigie/pull/3","state":"open","user":{"login":"sperano"},` +
		`"head":{"ref":"kelos/vikunja-9","sha":"abc","repo":{"full_name":"sperano/vigie"}},"base":{"ref":"main"},"labels":[{"name":"kelos:gpt-sol"}]}`
	repo := `"repository":{"full_name":"sperano/vigie"}`
	review, err := ParseGitHub(github("pull_request_review", "g2", `{"action":"submitted",`+repo+`,"review":{"id":21,"node_id":"PRR_21","body":"b","html_url":"u21"},`+pr+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if review.RequestKey != "github/sperano/vigie/r/21" || review.GitHub.CommentKind != CommentKindReview ||
		!review.GitHub.OnPullRequest || review.GitHub.CommentNodeID != "PRR_21" || review.GitHub.CommentURL != "u21" {
		t.Errorf("review %+v", review)
	}
	inline, err := ParseGitHub(github("pull_request_review_comment", "g3", `{"action":"created",`+repo+`,"comment":{"id":31,"node_id":"PRRC_31","body":"b","html_url":"u31","pull_request_review_id":21},`+pr+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if inline.RequestKey != "github/sperano/vigie/l/31" || inline.GitHub.CommentNodeID != "PRRC_31" ||
		inline.GitHub.CommentURL != "u31" || inline.GitHub.ReviewID != 21 {
		t.Errorf("inline request key %q", inline.RequestKey)
	}
	opened, err := ParseGitHub(github("pull_request", "g4", `{"action":"opened",`+repo+`,`+pr+`}`))
	if err != nil {
		t.Fatal(err)
	}
	want := PullRequest{Repository: "sperano/vigie", Number: 3, Title: "Fix", URL: "https://github.com/sperano/vigie/pull/3",
		State: "open", Author: "sperano",
		HeadRef: "kelos/vikunja-9", HeadSHA: "abc", HeadRepo: "sperano/vigie", BaseRef: "main", Labels: []string{"kelos:gpt-sol"}}
	got := opened.GitHub.PullRequest
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pull request %+v, want %+v", got, want)
	}
	if opened.RequestKey != "github/sperano/vigie/pull/3/opened/abc" {
		t.Errorf("pull request key %q", opened.RequestKey)
	}
}

func TestParseGitHubRefusals(t *testing.T) {
	repo := `"repository":{"full_name":"sperano/vigie"}`
	cases := map[string]struct {
		d    Delivery
		want error
	}{
		"unknown event":     {github("star", "g", `{}`), ErrUnsupported},
		"no delivery id":    {github("push", "", `{}`), ErrMalformed},
		"not json":          {github("push", "g", `{`), ErrMalformed},
		"no repository":     {github("push", "g", `{}`), ErrMalformed},
		"comment no body":   {github("issue_comment", "g", `{"action":"created",`+repo+`}`), ErrMalformed},
		"review no pr":      {github("pull_request_review", "g", `{`+repo+`,"review":{"id":1}}`), ErrMalformed},
		"inline no pr":      {github("pull_request_review_comment", "g", `{`+repo+`,"comment":{"id":1}}`), ErrMalformed},
		"pr no pr":          {github("pull_request", "g", `{`+repo+`}`), ErrMalformed},
		"closed no merged":  {github("pull_request", "g", `{"action":"closed",`+repo+`,"pull_request":{"number":3,"state":"closed"}}`), ErrMalformed},
		"pr marker not obj": {github("issue_comment", "g", `{"action":"created",`+repo+`,"issue":{"number":4,"pull_request":true},"comment":{"id":1,"body":"b"}}`), ErrMalformed},
		"comment id zero":   {github("issue_comment", "g", `{"action":"created",`+repo+`,"issue":{"number":4},"comment":{"id":0}}`), ErrMalformed},
		"issue no number":   {github("issue_comment", "g", `{"action":"created",`+repo+`,"issue":{},"comment":{"id":1}}`), ErrMalformed},
		"review id zero":    {github("pull_request_review", "g", `{`+repo+`,"review":{"id":0},"pull_request":{"number":3}}`), ErrMalformed},
		"inline id zero":    {github("pull_request_review_comment", "g", `{`+repo+`,"comment":{},"pull_request":{"number":3}}`), ErrMalformed},
		"pr no number":      {github("pull_request", "g", `{`+repo+`,"pull_request":{"state":"open"}}`), ErrMalformed},
	}
	for name, c := range cases {
		if _, err := ParseGitHub(c.d); !errors.Is(err, c.want) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
	push, err := ParseGitHub(github("push", "g", `{`+repo+`}`))
	if err != nil || push.Kind != GitHubPush || push.RequestKey != "" {
		t.Errorf("push %+v err %v", push, err)
	}
}

func TestTaskHelpers(t *testing.T) {
	task := Task{Labels: []string{"chain"}, Buckets: []string{"Doing"}}
	if !task.HasLabel("chain") || task.HasLabel("discuss") || !task.InBucket("Doing") || task.InBucket("To-Do") {
		t.Errorf("helpers on %+v", task)
	}
	if !(PullRequest{Labels: []string{"kelos:no-review"}}).HasLabel("kelos:no-review") {
		t.Error("PullRequest.HasLabel")
	}
}

func TestParseGitHubClosedPullRequestMerged(t *testing.T) {
	repo := `"repository":{"full_name":"sperano/vigie"}`
	for _, merged := range []bool{true, false} {
		body := fmt.Sprintf(`{"action":"closed",%s,"pull_request":{"number":3,"state":"closed","merged":%t}}`, repo, merged)
		ev, err := ParseGitHub(github("pull_request", "g", body))
		if err != nil {
			t.Fatalf("merged %t: %v", merged, err)
		}
		if ev.GitHub.PullRequest.Merged != merged {
			t.Errorf("merged = %t, want %t", ev.GitHub.PullRequest.Merged, merged)
		}
	}
}

func TestParseBySource(t *testing.T) {
	if _, err := Parse(Delivery{Source: SourceWikiComment, Body: []byte(`{}`)}); !errors.Is(err, ErrUnsupported) ||
		!strings.Contains(err.Error(), `"wiki-comment"`) {
		t.Fatalf("Parse(wiki-comment) = %v, want unsupported naming the source", err)
	}
	ev, err := Parse(Delivery{Source: SourceVikunja,
		Body: []byte(`{"event_name":"task.comment.created","data":{"task":{"id":1},"comment":{"id":2}}}`)})
	if err != nil || ev.Kind != VikunjaTaskCommentCreated {
		t.Fatalf("Parse(vikunja) = %+v, %v", ev, err)
	}
	ev, err = Parse(Delivery{Source: SourceGitHub, Headers: map[string]string{GitHubEventHeader: "push",
		GitHubDeliveryHeader: "g"}, Body: []byte(`{"repository":{"full_name":"a/b"}}`)})
	if err != nil || ev.Kind != GitHubPush {
		t.Fatalf("Parse(github) = %+v, %v", ev, err)
	}
}

func TestParseVikunjaSynthetic(t *testing.T) {
	ev, err := ParseVikunja([]byte(`{"event_name":"task.updated","data":{"task":{"id":7,"project_id":3},` +
		`"gordon":{"requeue":"vikunja/task.updated/5/x","depth":1}}}`))
	if err != nil || ev.Vikunja.Synthetic == nil || ev.Vikunja.Synthetic.Requeue != "vikunja/task.updated/5/x" ||
		ev.Vikunja.Synthetic.Depth != 1 {
		t.Fatalf("synthetic %+v, %v", ev.Vikunja, err)
	}
	_, err = ParseVikunja([]byte(`{"event_name":"task.updated","data":{"task":{"id":7,"project_id":3},` +
		`"gordon":{"requeue":"k","depth":-1000000}}}`))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("negative depth: %v, want ErrMalformed", err)
	}
	plain, err := ParseVikunja([]byte(`{"event_name":"task.updated","data":{"task":{"id":7,"project_id":3}}}`))
	if err != nil || plain.Vikunja.Synthetic != nil || plain.DeliveryKey == ev.DeliveryKey {
		t.Fatalf("plain delivery %+v, %v: no marker, its own key", plain.Vikunja, err)
	}
}

// TestParseGitHubConflict: only a synthetic delivery's Workspace counts;
// the base SHA is kept.
func TestParseGitHubConflict(t *testing.T) {
	body := `{"action":"gordon-conflict","repository":{"full_name":"sperano/vigie"},"gordon":{"workspace":"vigie"},` +
		`"pull_request":{"number":3,"state":"open","user":{"login":"sperano"},"head":{"ref":"kelos/vikunja-9","sha":"abc"},` +
		`"base":{"ref":"main","sha":"b45e"}}}`
	d := github("pull_request", "g5", body)
	webhook, err := ParseGitHub(d)
	if err != nil {
		t.Fatal(err)
	}
	if webhook.GitHub.Synthetic || webhook.GitHub.ConflictWorkspace != "" || webhook.GitHub.PullRequest.BaseSHA != "b45e" {
		t.Errorf("webhook %+v", webhook.GitHub)
	}
	d.Headers[SyntheticHeader] = "reconcile"
	synthetic, err := ParseGitHub(d)
	if err != nil {
		t.Fatal(err)
	}
	if !synthetic.GitHub.Synthetic || synthetic.GitHub.ConflictWorkspace != "vigie" || synthetic.Action != ActionConflict ||
		synthetic.RequestKey != "github/sperano/vigie/pull/3/gordon-conflict/abc" {
		t.Errorf("synthetic %+v, request %q", synthetic.GitHub, synthetic.RequestKey)
	}
}
