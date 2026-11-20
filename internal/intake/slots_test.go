package intake

import (
	"context"
	"testing"
	"time"
)

func TestSlotsBoundTotalAndPeer(t *testing.T) {
	const total = 8
	s := NewSlots(total)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	per := total / PeerShares
	for range per {
		if !s.Acquire(context.Background(), "a") {
			t.Fatal("peer a refused within its share")
		}
	}
	if s.Acquire(expired, "a") {
		t.Fatal("peer a took more than its share")
	}
	for _, p := range []string{"b", "c", "d"} {
		for range per {
			if !s.Acquire(context.Background(), p) {
				t.Fatalf("peer %s refused within its share", p)
			}
		}
	}
	if s.Acquire(expired, "e") {
		t.Fatal("a new peer took a slot over the total")
	}
	s.Release("b")
	if !s.Acquire(context.Background(), "e") {
		t.Fatal("freed slot not available")
	}
}

// waitUntil polls cond; the slots expose no event for a queued waiter.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// acquireAsync waits for a slot in a goroutine; the test's cleanup
// cancels a waiter still queued and joins it.
func acquireAsync(t *testing.T, s *Slots, peer string) chan bool {
	got := make(chan bool, 1)
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		got <- s.Acquire(ctx, peer)
	}()
	return got
}

func TestSlotsWaiterWakesOnRelease(t *testing.T) {
	s := NewSlots(1)
	s.Acquire(context.Background(), "a")
	got := acquireAsync(t, s, "b")
	waitUntil(t, func() bool { return s.waiting() == 1 })
	s.Release("a")
	if !<-got {
		t.Fatal("waiter not woken by the release")
	}
	if s.inUse != 1 || s.held["b"] != 1 {
		t.Fatalf("slot not handed to the waiter: in use %d, held %v", s.inUse, s.held)
	}
}

func TestSlotsReleaseWakesOnlyAWaiterWithRoom(t *testing.T) {
	s := NewSlots(8) // two per peer
	for _, p := range []string{"a", "a", "b", "b", "c", "c", "d", "d"} {
		s.Acquire(context.Background(), p)
	}
	full := acquireAsync(t, s, "a") // a is at its share whatever is released
	waitUntil(t, func() bool { return s.waiting() == 1 })
	other := acquireAsync(t, s, "e")
	waitUntil(t, func() bool { return s.waiting() == 2 })
	s.Release("b")
	if !<-other {
		t.Fatal("the waiter with room was not granted the slot")
	}
	select {
	case <-full:
		t.Fatal("a waiter at its share was woken")
	default:
	}
	if s.waiting() != 1 {
		t.Fatalf("%d waiters, want only peer a's", s.waiting())
	}
}

func TestSlotsCancelledWaiterLeavesTheQueue(t *testing.T) {
	s := NewSlots(1)
	s.Acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool, 1)
	go func() { got <- s.Acquire(ctx, "b") }()
	waitUntil(t, func() bool { return s.waiting() == 1 })
	cancel()
	if <-got {
		t.Fatal("cancelled waiter got a slot")
	}
	s.Release("a")
	if s.waiting() != 0 || s.inUse != 0 {
		t.Fatalf("cancelled waiter kept: %d waiting, %d in use", s.waiting(), s.inUse)
	}
}

func TestSlotsEndedContextTakesNoFreeSlot(t *testing.T) {
	s := NewSlots(4)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if s.Acquire(ended, "a") || s.inUse != 0 {
		t.Fatalf("an ended context took a free slot (%d in use)", s.inUse)
	}
}

func TestSlotsMinimumOnePerPeer(t *testing.T) {
	s := NewSlots(2) // 2/PeerShares rounds to 0
	if !s.Acquire(context.Background(), "a") {
		t.Fatal("peer refused its single slot")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if s.Acquire(expired, "a") {
		t.Fatal("peer took a second slot")
	}
}
