package activity

import (
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/sarataha/warmrunners/internal/predictor"
)

// withNow pins the feed's clock to fn for the duration of the caller's use.
// It type-asserts to *inMemoryFeed and delegates to the package-private
// setNow method so tests never touch feed internals directly.
func withNow(f EventFeed, fn func() time.Time) {
	im, ok := f.(*inMemoryFeed)
	if !ok {
		return
	}
	im.setNow(fn)
}

func TestInMemoryEventFeed_RecordJobUpdatesSnapshot(t *testing.T) {
	f := NewInMemoryEventFeed(logr.Discard())

	f.RecordJob("acme/widgets", []string{"self-hosted", "linux"})

	perLabelSet, lastEvent := f.Snapshot("acme/widgets")
	if perLabelSet == nil {
		t.Fatalf("expected non-nil perLabelSet")
	}
	key := predictor.LabelSetKey([]string{"self-hosted", "linux"})
	if got := perLabelSet[key]; got != 1 {
		t.Errorf("perLabelSet[%q] = %d, want 1", key, got)
	}
	if lastEvent.IsZero() {
		t.Errorf("expected non-zero lastEvent")
	}
}

func TestInMemoryEventFeed_RecordPushMergesFanout(t *testing.T) {
	f := NewInMemoryEventFeed(logr.Discard())

	pushTime := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	jobTime := time.Date(2026, 7, 11, 10, 0, 5, 0, time.UTC)

	withNow(f, func() time.Time { return pushTime })
	f.RecordPush("acme/widgets", "deadbeef")

	withNow(f, func() time.Time { return jobTime })
	f.RecordJob("acme/widgets", []string{"linux"})

	perLabelSet, lastEvent := f.Snapshot("acme/widgets")
	if len(perLabelSet) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(perLabelSet), perLabelSet)
	}
	key := predictor.LabelSetKey([]string{"linux"})
	if got := perLabelSet[key]; got != 1 {
		t.Errorf("perLabelSet[%q] = %d, want 1", key, got)
	}
	if !lastEvent.Equal(jobTime) {
		t.Errorf("lastEvent = %v, want %v", lastEvent, jobTime)
	}
}

func TestInMemoryEventFeed_SnapshotZeroWhenEmpty(t *testing.T) {
	f := NewInMemoryEventFeed(logr.Discard())

	perLabelSet, lastEvent := f.Snapshot("does/not/exist")
	if perLabelSet != nil {
		t.Errorf("expected nil perLabelSet, got %v", perLabelSet)
	}
	if !lastEvent.IsZero() {
		t.Errorf("expected zero lastEvent, got %v", lastEvent)
	}
}

func TestInMemoryEventFeed_Concurrent_race(t *testing.T) {
	f := NewInMemoryEventFeed(logr.Discard())

	repos := []string{"acme/widgets", "acme/gadgets", "acme/gizmos"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := repos[i%len(repos)]
			switch i % 3 {
			case 0:
				f.RecordPush(repo, "sha123")
			case 1:
				f.RecordJob(repo, []string{"self-hosted", "linux"})
			case 2:
				_, _ = f.Snapshot(repo)
			}
		}(i)
	}
	wg.Wait()
}
