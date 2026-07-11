package webhook

import (
	"container/list"
	"sync"
	"time"
)

// ReplayGuard tracks recently seen webhook delivery IDs to detect replays.
// It is an LRU cache bounded by size, with entries additionally expiring
// after ttl. Safe for concurrent use.
type ReplayGuard struct {
	mu     sync.Mutex
	size   int
	ttl    time.Duration
	lru    *list.List
	lookup map[string]*list.Element
	now    func() time.Time
}

type replayEntry struct {
	key        string
	insertedAt time.Time
}

// NewReplayGuard constructs a ReplayGuard bounded to size entries, with
// entries expiring after ttl.
func NewReplayGuard(size int, ttl time.Duration) *ReplayGuard {
	return &ReplayGuard{
		size:   size,
		ttl:    ttl,
		lru:    list.New(),
		lookup: make(map[string]*list.Element),
		now:    time.Now,
	}
}

// setNow overrides the guard's clock for testing.
func (g *ReplayGuard) setNow(f func() time.Time) {
	g.now = f
}

// Seen records deliveryID and reports whether it has already been seen
// within the TTL window. The first call for a given ID returns false;
// subsequent calls within the TTL return true. Entries are evicted on
// TTL expiry or when the LRU size cap is exceeded.
func (g *ReplayGuard) Seen(deliveryID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()

	if elem, ok := g.lookup[deliveryID]; ok {
		entry := elem.Value.(*replayEntry)
		if now.Sub(entry.insertedAt) <= g.ttl {
			return true
		}
		// Expired: fall through and re-insert as new.
		g.lru.Remove(elem)
		delete(g.lookup, deliveryID)
	}

	if g.lru.Len() >= g.size {
		oldest := g.lru.Back()
		if oldest != nil {
			g.lru.Remove(oldest)
			delete(g.lookup, oldest.Value.(*replayEntry).key)
		}
	}

	elem := g.lru.PushFront(&replayEntry{key: deliveryID, insertedAt: now})
	g.lookup[deliveryID] = elem
	return false
}
