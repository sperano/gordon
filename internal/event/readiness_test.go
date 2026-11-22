package event

import (
	"errors"
	"testing"
)

func TestParseGitHubCheckRun(t *testing.T) {
	ev, err := ParseGitHub(github("check_run", "g9", `{"action":"completed","sender":{"login":"github-actions[bot]"},`+
		`"repository":{"full_name":"sperano/vigie"},"check_run":{"id":3,"head_sha":"0123abcd","pull_requests":[{"number":4}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != GitHubCheckRun || ev.Action != ActionCompleted || ev.DeliveryKey != "github/delivery/g9" || ev.RequestKey != "" ||
		ev.GitHub.CheckRunHeadSHA != "0123abcd" || ev.GitHub.OnPullRequest {
		t.Errorf("event %+v github %+v", ev, ev.GitHub)
	}
	for _, body := range []string{`{"action":"completed","repository":{"full_name":"sperano/vigie"}}`,
		`{"action":"completed","repository":{"full_name":"sperano/vigie"},"check_run":{"id":3}}`} {
		if _, err := ParseGitHub(github("check_run", "g", body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want malformed", body, err)
		}
	}
}

func TestParseGitHubReadinessDelivery(t *testing.T) {
	d := github("pull_request", "gordon-readiness/x", `{"action":"gordon-readiness","sender":{"login":"sperano"},`+
		`"repository":{"full_name":"sperano/vigie"},"pull_request":{"number":4,"head":{"ref":"kelos/vikunja-5","sha":"abc"}}}`)
	d.Headers[SyntheticHeader] = "reconcile"
	ev, err := ParseGitHub(d)
	if err != nil || ev.Action != ActionReadiness || !ev.GitHub.Synthetic || ev.GitHub.PullRequest.Number != 4 ||
		ev.RequestKey != "github/sperano/vigie/pull/4/gordon-readiness/abc" {
		t.Errorf("event %+v, %v", ev, err)
	}
}
