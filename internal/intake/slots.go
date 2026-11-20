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
	// waiters queue in arrival order; a release hands its slot to the
	// first one whose peer has room, and wakes only that one.
	waiters []*waiter
}

type waiter struct {
	peer    string
	granted chan struct{}
}

// NewSlots returns total slots.
func NewSlots(total int) *Slots {
	return &Slots{total: total, perPeer: max(1, total/PeerShares), held: map[string]int{}}
}

// Acquire takes a slot for peer; false when ctx ended first (a context
// already ended takes none).
func (s *Slots) Acquire(ctx context.Context, peer string) bool {
	if ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	if s.room(peer) {
		s.take(peer)
		s.mu.Unlock()
		return true
	}
	w := &waiter{peer: peer, granted: make(chan struct{})}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()
	select {
	case <-w.granted:
		return true
	case <-ctx.Done():
	}
	s.mu.Lock()
	for i, o := range s.waiters {
		if o == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.mu.Unlock()
			return false
		}
	}
	s.mu.Unlock()
	// Granted while ctx ended: hand the slot on.
	s.Release(peer)
	return false
}

// Release returns peer's slot, handing it to the first waiter with room.
func (s *Slots) Release(peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse--
	if s.held[peer]--; s.held[peer] <= 0 {
		delete(s.held, peer)
	}
	for i, w := range s.waiters {
		if s.room(w.peer) {
			s.take(w.peer)
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			close(w.granted)
			return
		}
	}
}

func (s *Slots) room(peer string) bool { return s.inUse < s.total && s.held[peer] < s.perPeer }

func (s *Slots) take(peer string) {
	s.inUse++
	s.held[peer]++
}

// waiting is how many Acquire calls are queued.
func (s *Slots) waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}
