// Package worker decides stored deliveries and performs the actions of
// the decisions Gordon executes. Each piece claims its rows under a lease
// (see package store), so any number of worker replicas may run, and one
// that dies mid-item leaves the row to the next claim once its lease
// expires.
package worker

import "time"

// Backoff is a capped exponential retry delay.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Delay is the wait before the attempt after attempt (1-based):
// Base, 2·Base, 4·Base, ... capped at Max.
func (b Backoff) Delay(attempt int) time.Duration {
	d := b.Base
	for i := 1; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	return min(d, b.Max)
}
