package webhook

import (
	"sync"
	"testing"
	"time"
)

func TestReplayGuard_FirstSeenReturnsFalse(t *testing.T) {
	g := NewReplayGuard(10, time.Hour)

	if got := g.Seen("delivery-1"); got {
		t.Fatalf("Seen() on first occurrence = %v, want false", got)
	}
}

func TestReplayGuard_DuplicateReturnsTrue(t *testing.T) {
	g := NewReplayGuard(10, time.Hour)

	if got := g.Seen("delivery-1"); got {
		t.Fatalf("Seen() on first occurrence = %v, want false", got)
	}
	if got := g.Seen("delivery-1"); !got {
		t.Fatalf("Seen() on duplicate = %v, want true", got)
	}
}

func TestReplayGuard_TTLExpiry(t *testing.T) {
	g := NewReplayGuard(10, time.Minute)

	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.setNow(func() time.Time { return current })

	if got := g.Seen("delivery-1"); got {
		t.Fatalf("Seen() on first occurrence = %v, want false", got)
	}

	// Advance past the TTL.
	current = current.Add(2 * time.Minute)

	if got := g.Seen("delivery-1"); got {
		t.Fatalf("Seen() after TTL expiry = %v, want false", got)
	}
}

func TestReplayGuard_LRUEviction(t *testing.T) {
	g := NewReplayGuard(2, time.Hour)

	g.Seen("delivery-1")
	g.Seen("delivery-2")
	g.Seen("delivery-3") // should evict delivery-1 (oldest)

	if got := g.Seen("delivery-1"); got {
		t.Fatalf("Seen(delivery-1) after eviction = %v, want false (evicted, treated as new)", got)
	}
	if got := g.Seen("delivery-3"); !got {
		t.Fatalf("Seen(delivery-3) = %v, want true (still present)", got)
	}
}

func TestReplayGuard_Concurrent(t *testing.T) {
	g := NewReplayGuard(1000, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				g.Seen("delivery-concurrent")
			}
		}()
	}
	wg.Wait()
}
