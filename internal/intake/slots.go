package intake

import (
	"context"
	"sync"
)

// PeerShares is how many client addresses it takes to fill the slots:
// one address holds at most total/PeerShares (at least one), as the
// legacy bounded_http.Slots.
const PeerShares = 4

// Slots bounds the requests being read and stored at once, in all and
// per client address. Acquire waits for room until its context ends, so
// an address holding its share delays only its own requests.
type Slots struct {
	mu      sync.Mutex
	total   int
	perPeer int
	inUse   int
	held    map[string]int
	changed chan struct{}
}

// NewSlots returns total slots.
func NewSlots(total int) *Slots {
	return &Slots{total: total, perPeer: max(1, total/PeerShares), held: map[string]int{},
		changed: make(chan struct{})}
}

// Acquire takes a slot for peer; false when ctx ended first.
func (s *Slots) Acquire(ctx context.Context, peer string) bool {
	for {
		s.mu.Lock()
		if s.inUse < s.total && s.held[peer] < s.perPeer {
			s.inUse++
			s.held[peer]++
			s.mu.Unlock()
			return true
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// Release returns peer's slot and wakes every waiter: they wait for
// different peers, so the one woken may not be the one with room.
func (s *Slots) Release(peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse--
	if s.held[peer]--; s.held[peer] <= 0 {
		delete(s.held, peer)
	}
	close(s.changed)
	s.changed = make(chan struct{})
}
