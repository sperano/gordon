// Package sillage reads a Vikunja task's estimated API-equivalent cost
// from Sillage's read API (`GET /api/v1/cost/task/<vikunja host>/<id>`,
// sperano/sillage internal/cost.TaskCost, from Gordon 11). Only the fields
// the completion notices need are decoded; a response missing the price
// table's version or the task total's status is an incompatible API, not
// an estimate.
//
// The estimate is what the usage would have cost at the providers'
// public list prices. It is not what was billed. An absent estimatedUSD
// is unknown, never zero.
package sillage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// CostPath is the cost endpoint's path, less the task's host and id.
const CostPath = "/api/v1/cost/task/"

// maxResponseBytes caps one cost response: a task's runs, attempts,
// models and threads.
const maxResponseBytes = 8 << 20

// Statuses of a breakdown (cost.Status).
const (
	StatusComplete = "complete"
	StatusPartial  = "partial"
	StatusUnknown  = "unknown"
	StatusNone     = "none"
)

// statuses are the statuses this client understands.
var statuses = []string{StatusComplete, StatusPartial, StatusUnknown, StatusNone}

// Errors of a cost read. ErrNotFound is a task Sillage has no story of
// yet (its collector has not read it); ErrIncompatible is an answer this
// client cannot read as the contract. Anything else (a transport error,
// a non-2xx status) is Sillage unavailable.
var (
	ErrNotFound     = errors.New("sillage has no story of the task")
	ErrIncompatible = errors.New("sillage cost response does not match the contract")
)

// StatusError is a Sillage answer outside 2xx other than 404.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("sillage cost: HTTP %d", e.Status) }

// TaskCost is the part of cost.TaskCost the notices read.
type TaskCost struct {
	Prices      Prices       `json:"prices"`
	Assumptions []Assumption `json:"assumptions"`
	Feed        Feed         `json:"feed"`
	// Final: every run ended and no more usage is expected.
	Final bool      `json:"final"`
	Total Breakdown `json:"total"`
	Runs  []Run     `json:"runs"`
}

// Prices names the price table an estimate used.
type Prices struct {
	Version    string `json:"version"`
	VerifiedAt string `json:"verifiedAt"`
}

// Assumption is one stated assumption of the estimate.
type Assumption struct {
	Code string `json:"code"`
	Text string `json:"text"`
}

// Feed is what Sillage's usage feed has read.
type Feed struct {
	On bool `json:"on"`
}

// Breakdown is an estimate by category, with its status.
type Breakdown struct {
	Status string `json:"status"`
	// EstimatedUSD is a decimal string; "" when nothing was priced.
	EstimatedUSD   string     `json:"estimatedUSD,omitempty"`
	ReportedUSD    string     `json:"reportedUSD,omitempty"`
	Requests       int        `json:"requests"`
	FailedRequests int        `json:"failedRequests"`
	Categories     []Category `json:"categories"`
	Notes          []string   `json:"notes,omitempty"`
}

// Category is one way tokens are priced.
type Category struct {
	Category     string `json:"category"`
	Tokens       int64  `json:"tokens"`
	EstimatedUSD string `json:"estimatedUSD,omitempty"`
	Unpriced     bool   `json:"unpriced,omitempty"`
}

// Run is one run's estimate; Key.UID is its Kelos Task's uid.
type Run struct {
	Key struct {
		UID string `json:"uid,omitempty"`
	} `json:"key"`
	TaskName string `json:"taskName,omitempty"`
	Outcome  string `json:"outcome"`
	// Final: the run ended and the feed read far enough past its end.
	Final bool `json:"final"`
	Breakdown
	Attempts []struct {
		Pod string `json:"pod"`
	} `json:"attempts"`
	Threads []struct {
		Agent string `json:"agent,omitempty"`
	} `json:"threads"`
}

// RunByUID returns the run of a Kelos Task uid.
func (c TaskCost) RunByUID(uid string) (Run, bool) {
	if uid == "" {
		return Run{}, false
	}
	for _, r := range c.Runs {
		if r.Key.UID == uid {
			return r, true
		}
	}
	return Run{}, false
}

// Client reads Sillage's cost API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// TaskCost reads the cost of the Vikunja task id on the Vikunja host.
func (c *Client) TaskCost(ctx context.Context, host string, id int64) (TaskCost, error) {
	u := strings.TrimRight(c.BaseURL, "/") + CostPath + url.PathEscape(host) + "/" + strconv.FormatInt(id, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return TaskCost{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return TaskCost{}, fmt.Errorf("sillage cost: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return TaskCost{}, ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return TaskCost{}, &StatusError{Status: resp.StatusCode}
	}
	return decode(io.LimitReader(resp.Body, maxResponseBytes+1))
}

func decode(r io.Reader) (TaskCost, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return TaskCost{}, fmt.Errorf("sillage cost: %w", err)
	}
	if len(data) > maxResponseBytes {
		return TaskCost{}, fmt.Errorf("%w: response over %d bytes", ErrIncompatible, maxResponseBytes)
	}
	var out TaskCost
	if err := json.Unmarshal(data, &out); err != nil {
		return TaskCost{}, fmt.Errorf("%w: %v", ErrIncompatible, err)
	}
	return out, out.validate()
}

// validate refuses an answer without the fields every notice states.
func (c TaskCost) validate() error {
	if c.Prices.Version == "" || c.Prices.VerifiedAt == "" {
		return fmt.Errorf("%w: no price table version or date", ErrIncompatible)
	}
	if !validStatus(c.Total.Status) {
		return fmt.Errorf("%w: total status %q", ErrIncompatible, c.Total.Status)
	}
	for _, r := range c.Runs {
		if !validStatus(r.Status) {
			return fmt.Errorf("%w: run %s status %q", ErrIncompatible, r.Key.UID, r.Status)
		}
	}
	return nil
}

func validStatus(s string) bool { return slices.Contains(statuses, s) }
