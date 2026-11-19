// Package replay runs sanitized legacy deliveries through the router.
// A fixture holds one delivery as a legacy receiver got it, the fresh
// upstream state Gordon would read, the ledger of handled work, what the
// legacy spawners did with it, and the exact decision Gordon must reach.
// Fixtures carry no real secret: the harness signs each body with
// FixtureKey unless the fixture asks for a missing or wrong signature.
package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
	"github.com/sperano/gordon/internal/profile"
	"github.com/sperano/gordon/internal/rollout"
	"github.com/sperano/gordon/internal/route"
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
	Ownership   *rollout.Table `json:"ownership,omitempty"`
	Legacy      Legacy         `json:"legacy"`
	Expect      route.Decision `json:"expect"`
}

// Delivery is the fixture's raw delivery.
type Delivery struct {
	Source    event.Source      `json:"source"`
	Headers   map[string]string `json:"headers,omitempty"`
	Signature string            `json:"signature,omitempty"`
	Body      json.RawMessage   `json:"body"`
}

// Legacy is what the legacy spawners did with the delivery: the Kelos
// Task names they created. Divergence, when set, explains why Gordon's
// runs deliberately differ.
type Legacy struct {
	Tasks      []string `json:"tasks"`
	Divergence string   `json:"divergence,omitempty"`
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
		f.Name = filepath.Base(p)
		out = append(out, f)
	}
	return out, nil
}

// Router is the router fixtures run against: the legacy defaults, the
// fixture key for every source, and the fixture's ownership table.
func (f Fixture) Router() route.Router {
	keys := auth.Keys{}
	for _, s := range []event.Source{event.SourceVikunja, event.SourceGitHub, event.SourceWikiComment, event.SourceImageBuild} {
		keys[s] = FixtureKey
	}
	r := route.Router{Config: profile.Defaults(), Keys: keys}
	if f.Ownership != nil {
		r.Ownership = *f.Ownership
	}
	return r
}

// Build turns the fixture's delivery into the signed bytes a receiver gets.
func (f Fixture) Build() (event.Delivery, error) {
	var body bytes.Buffer
	if err := json.Compact(&body, f.Delivery.Body); err != nil {
		return event.Delivery{}, fmt.Errorf("%s: body: %w", f.Name, err)
	}
	headers := map[string]string{}
	for k, v := range f.Delivery.Headers {
		headers[k] = v
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
	if key != nil {
		name, value, err := auth.Sign(f.Delivery.Source, key, body.Bytes())
		if err != nil {
			return event.Delivery{}, fmt.Errorf("%s: %w", f.Name, err)
		}
		headers[name] = value
	}
	return event.Delivery{Source: f.Delivery.Source, Headers: headers, Body: body.Bytes()}, nil
}

// Run decides the fixture's delivery.
func (f Fixture) Run() (route.Decision, error) {
	d, err := f.Build()
	if err != nil {
		return route.Decision{}, err
	}
	return f.Router().Decide(d, f.State, f.Ledger), nil
}

// RunNames lists the Task names a decision submits, in order.
func RunNames(d route.Decision) []string {
	names := []string{}
	for _, fd := range d.Families {
		for _, r := range fd.Runs {
			names = append(names, r.Name)
		}
	}
	return names
}
