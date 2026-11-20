package worker

import "sync"

// Limiter bounds concurrent work per key (a project, a provider). A key
// without its own limit takes Default; a limit <= 0 is unbounded. It never
// blocks: a full key refuses, and the caller defers the row so it stays
// durably queued instead of holding a worker.
type Limiter struct {
	Default int
	Limits  map[string]int

	mu    sync.Mutex
	inUse map[string]int
}

func (l *Limiter) limit(key string) int {
	if n, ok := l.Limits[key]; ok {
		return n
	}
	return l.Default
}

// TryAcquire takes a slot of key; false when the key is full.
func (l *Limiter) TryAcquire(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.limit(key)
	if n <= 0 {
		return true
	}
	if l.inUse == nil {
		l.inUse = map[string]int{}
	}
	if l.inUse[key] >= n {
		return false
	}
	l.inUse[key]++
	return true
}

// Release returns a slot TryAcquire took.
func (l *Limiter) Release(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit(key) <= 0 {
		return
	}
	if l.inUse[key]--; l.inUse[key] <= 0 {
		delete(l.inUse, key)
	}
}
