package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sperano/gordon/internal/route"
)

const fixtureDir = "testdata"

// minFixtures is the committed fixture count: deleting one fails, and
// the glob cannot silently match nothing.
const minFixtures = 32

func load(t *testing.T) []Fixture {
	t.Helper()
	fixtures, err := Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < minFixtures {
		t.Fatalf("loaded %d fixtures, want at least %d", len(fixtures), minFixtures)
	}
	return fixtures
}

func canonical(t *testing.T, d route.Decision) string {
	t.Helper()
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestReplayDecisions checks every fixture's exact decision.
func TestReplayDecisions(t *testing.T) {
	for _, f := range load(t) {
		t.Run(f.Name, func(t *testing.T) {
			if f.Ownership != nil {
				if err := f.Ownership.Validate(); err != nil {
					t.Fatalf("fixture ownership: %v", err)
				}
			}
			got, err := f.Run()
			if err != nil {
				t.Fatal(err)
			}
			if g, w := canonical(t, got), canonical(t, f.Expect); g != w {
				t.Errorf("decision differs\n got: %s\nwant: %s", g, w)
			}
		})
	}
}

// TestLegacyParity checks that Gordon submits exactly the Task names the
// legacy spawners created, unless the fixture explains the divergence.
func TestLegacyParity(t *testing.T) {
	for _, f := range load(t) {
		t.Run(f.Name, func(t *testing.T) {
			got, err := f.Run()
			if err != nil {
				t.Fatal(err)
			}
			names, legacy := RunNames(got), f.Legacy.Tasks
			if legacy == nil {
				legacy = []string{}
			}
			if f.Legacy.Divergence == "" && !slices.Equal(names, legacy) {
				t.Errorf("runs %v, legacy created %v and no divergence is recorded", names, legacy)
			}
		})
	}
}

// TestFixturesCoverOutcomes keeps a representative fixture for every
// outcome the acceptance names.
func TestFixturesCoverOutcomes(t *testing.T) {
	seen := map[route.Outcome]bool{}
	for _, f := range load(t) {
		seen[f.Expect.Outcome] = true
	}
	for _, o := range []route.Outcome{route.Dispatch, route.Act, route.Ignore, route.Reject,
		route.Duplicate, route.Stale, route.Conflict} {
		if !seen[o] {
			t.Errorf("no fixture expects %s", o)
		}
	}
}

func TestBuildSignatureModes(t *testing.T) {
	f := Fixture{Name: "x", Delivery: Delivery{Source: "vikunja", Body: json.RawMessage(`{"a": 1}`)}}
	d, err := f.Build()
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Body) != `{"a":1}` {
		t.Errorf("body %q, want compacted", d.Body)
	}
	// HMAC-SHA256 of {"a":1} under FixtureKey, computed with Python's hmac.
	const want = "e61b6c517214507b603ebbfa23647a3ae5f31905e187d49856c3a8bb524b415f"
	if got := d.Header("X-Vikunja-Signature"); got != want {
		t.Errorf("signature %q, want %q", got, want)
	}
	f.Delivery.Signature = SignatureMissing
	f.Delivery.Headers = map[string]string{"x-vikunja-signature": want, "X-Other": "kept"}
	d, err = f.Build()
	if err != nil {
		t.Fatal(err)
	}
	if d.Header("X-Vikunja-Signature") != "" || d.Header("X-Other") != "kept" {
		t.Errorf("missing-signature headers %v, want only X-Other", d.Headers)
	}
	f.Delivery.Signature = "bogus"
	if _, err := f.Build(); err == nil || err.Error() != `x: unknown signature mode "bogus"` {
		t.Errorf("unknown signature mode: err %v", err)
	}
}

func TestLoadRefusesTrailingData(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"description":"a"} {"description":"b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	want := filepath.Join(dir, "a.json") + ": trailing data after the fixture"
	if err == nil || err.Error() != want {
		t.Errorf("err %v, want %q", err, want)
	}
}
