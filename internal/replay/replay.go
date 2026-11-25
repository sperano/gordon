// Package replay runs sanitized deliveries through the router. A fixture
// holds one delivery as the receiver gets it, the fresh upstream state
// Gordon would read, the ledger of handled work, and the exact decision
// Gordon must reach.
// Fixtures carry no real secret: the harness signs each body with
// FixtureKey unless the fixture asks for a missing or wrong signature.
package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sperano/gordon/internal/action"
	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// FixtureKey signs every fixture delivery, for every source.
var FixtureKey = []byte("replay-fixture-key")

// wrongKey signs a fixture whose signature must be refused.
var wrongKey = []byte("not-the-fixture-key")

// Signature modes of a fixture delivery.
const (
	SignatureValid   = ""
	SignatureMissing = "missing"
	SignatureInvalid = "invalid"
)

// Fixture is one replayed delivery.
type Fixture struct {
	Name        string         `json:"-"`
	Description string         `json:"description"`
	Delivery    Delivery       `json:"delivery"`
	State       route.State    `json:"state"`
	Ledger      Ledger         `json:"ledger"`
	Expect      route.Decision `json:"expect"`
}

// Delivery is the fixture's raw delivery.
type Delivery struct {
	Source    event.Source      `json:"source"`
	Headers   map[string]string `json:"headers,omitempty"`
	Signature string            `json:"signature,omitempty"`
	Body      json.RawMessage   `json:"body"`
}

// Ledger is an in-memory route.Ledger.
type Ledger struct {
	Deliveries []string          `json:"deliveries,omitempty"`
	Requests   []string          `json:"requests,omitempty"`
	Runs       []route.RunRecord `json:"runs,omitempty"`
}

func (l Ledger) SeenDelivery(key string) bool   { return slices.Contains(l.Deliveries, key) }
func (l Ledger) HandledRequest(key string) bool { return slices.Contains(l.Requests, key) }

func (l Ledger) Run(name string) (route.RunRecord, bool) {
	for _, r := range l.Runs {
		if r.Name == name {
			return r, true
		}
	}
	return route.RunRecord{}, false
}

// TaskRuns returns the task's runs in the families, ordered by name as
// the store's ledger returns them.
func (l Ledger) TaskRuns(taskID int64, families []run.Family) []route.RunRecord {
	var out []route.RunRecord
	for _, r := range l.Runs {
		if r.TaskID == taskID && slices.Contains(families, r.Family) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b route.RunRecord) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// PRRuns returns the pull request's follow-up, review and conflict runs,
// ordered by name.
func (l Ledger) PRRuns(workspace string, pr int) []route.RunRecord {
	var out []route.RunRecord
	for _, r := range l.Runs {
		if slices.Contains(prFamilies, r.Family) && r.Workspace == workspace && r.PR == pr {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b route.RunRecord) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// prFamilies are the families whose runs work on a pull request.
var prFamilies = []run.Family{run.PRFollowUp, run.PRReview, run.PRConflict}

// Load reads every *.json fixture of dir, sorted by name.
func Load(dir string) ([]Fixture, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]Fixture, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var f Fixture
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: trailing data after the fixture", p)
		}
		f.Name = filepath.Base(p)
		out = append(out, f)
	}
	return out, nil
}

// Router is the router fixtures run against: the routing defaults and the
// fixture key for every source.
func (f Fixture) Router() route.Router {
	keys := auth.Keys{}
	for _, s := range []event.Source{event.SourceVikunja, event.SourceGitHub, event.SourceWikiComment, event.SourceImageBuild} {
		keys[s] = FixtureKey
	}
	return route.Router{Config: profile.Defaults(), Keys: keys}
}

// Build turns the fixture's delivery into the signed bytes a receiver gets.
func (f Fixture) Build() (event.Delivery, error) {
	var body bytes.Buffer
	if err := json.Compact(&body, f.Delivery.Body); err != nil {
		return event.Delivery{}, fmt.Errorf("%s: body: %w", f.Name, err)
	}
	key := FixtureKey
	switch f.Delivery.Signature {
	case SignatureMissing:
		key = nil
	case SignatureInvalid:
		key = wrongKey
	case SignatureValid:
	default:
		return event.Delivery{}, fmt.Errorf("%s: unknown signature mode %q", f.Name, f.Delivery.Signature)
	}
	headers := f.headers(key == nil)
	if key != nil {
		name, value, err := auth.Sign(f.Delivery.Source, key, body.Bytes())
		if err != nil {
			return event.Delivery{}, fmt.Errorf("%s: %w", f.Name, err)
		}
		headers[name] = value
	}
	return event.Delivery{Source: f.Delivery.Source, Headers: headers, Body: body.Bytes()}, nil
}

// headers copies the fixture's headers; unsigned drops any signature
// header they carry (case-insensitively), so a missing signature is
// missing whatever the fixture says.
func (f Fixture) headers(unsigned bool) map[string]string {
	sig, _ := auth.Header(f.Delivery.Source)
	headers := map[string]string{}
	for k, v := range f.Delivery.Headers {
		if !unsigned || !strings.EqualFold(k, sig) {
			headers[k] = v
		}
	}
	return headers
}

// Run decides the fixture's delivery.
func (f Fixture) Run() (route.Decision, error) {
	d, err := f.Build()
	if err != nil {
		return route.Decision{}, err
	}
	return f.Router().Decide(d, f.State, f.Ledger), nil
}

// WriteKeys lists the keys of a decision's writes, Task submissions
// aside, sorted and without repeats (two families may queue one write).
func WriteKeys(d route.Decision) []string {
	keys := []string{}
	for _, fd := range d.Families {
		for _, a := range fd.Actions {
			if a.Kind != action.SubmitTask && !slices.Contains(keys, a.Key) {
				keys = append(keys, a.Key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}
