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

func TestSlotsWaiterWakesOnRelease(t *testing.T) {
	s := NewSlots(1)
	s.Acquire(context.Background(), "a")
	got := make(chan bool)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		got <- s.Acquire(ctx, "b")
	}()
	s.Release("a")
	if !<-got {
		t.Fatal("waiter not woken by the release")
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
