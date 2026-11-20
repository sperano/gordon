package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sperano/gordon/internal/store"
)

func TestBound(t *testing.T) {
	if Bound("github", Sources) != "github" || Bound("../../etc", Sources) != Other || Bound("", Outcomes) != Other {
		t.Fatal("Bound kept a value outside its set or dropped one inside")
	}
	if Bound(None, Families()) != None || Bound("pr-review", Families()) != "pr-review" {
		t.Fatal("families set incomplete")
	}
}

func TestSetQueuesZeroesEmptyStates(t *testing.T) {
	m := New()
	m.SetQueues([]store.QueueStat{{Queue: store.QueueActions, State: store.ActionAmbiguous, Count: 3, OldestAge: time.Minute}})
	if v := testutil.ToFloat64(m.QueueDepth.WithLabelValues("actions", "ambiguous")); v != 3 {
		t.Fatalf("ambiguous depth = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.QueueOldest.WithLabelValues("actions", "ambiguous")); v != 60 {
		t.Fatalf("ambiguous age = %v, want 60", v)
	}
	m.SetQueues(nil)
	if v := testutil.ToFloat64(m.QueueDepth.WithLabelValues("actions", "ambiguous")); v != 0 {
		t.Fatalf("drained state depth = %v, want 0", v)
	}
	m.SetQueues([]store.QueueStat{{Queue: "mystery", State: "x", Count: 9}, {Queue: store.QueueInbox, State: "weird", Count: 2}})
	if testutil.CollectAndCount(m.QueueDepth) != len(QueueStates[store.QueueInbox])+len(QueueStates[store.QueueActions])+1 {
		t.Fatal("unknown queue or state grew the series beyond the fixed set plus other")
	}
	if v := testutil.ToFloat64(m.QueueDepth.WithLabelValues("inbox", Other)); v != 2 {
		t.Fatalf("unknown inbox state recorded as %v under other, want 2", v)
	}
}
