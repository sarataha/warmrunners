package activity

import (
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/sarataha/warmrunners/internal/predictor"
)

// EventFeed accepts near-real-time webhook events and merges them into a
// per-repo activity snapshot. The reconciler reads snapshots via Snapshot;
// events arriving after Snapshot was called are visible on the next call.
type EventFeed interface {
	RecordPush(repo, headSHA string)
	RecordJob(repo string, labels []string)
	// Snapshot returns the merged fanout per label-set for repo, plus the
	// timestamp of the most recent event. lastEvent is the zero time.Time
	// when no event has been recorded.
	Snapshot(repo string) (perLabelSet map[string]int, lastEvent time.Time)
}

// repoState is the per-repo accumulator held by inMemoryFeed.
type repoState struct {
	perLabelSet map[string]int
	lastEvent   time.Time
	// lastHeadSHA is the latest head SHA seen on a push. Not currently read
	// by callers; kept for future push->YAML refresh work and for
	// /metrics debugging.
	lastHeadSHA string
}

// inMemoryFeed is the in-process EventFeed implementation. It holds no
// GitHub API dependency: per-label-set fanout is derived solely from
// RecordJob calls, which carry deterministic ground truth from the webhook
// payload.
type inMemoryFeed struct {
	log logr.Logger
	mu  sync.RWMutex
	// repos is keyed by full repo name ("owner/name").
	repos map[string]*repoState
	// now is a test seam; nil means use time.Now.
	now func() time.Time
}

// NewInMemoryEventFeed constructs an EventFeed that keeps all state
// in-process, with no persistence across restarts.
func NewInMemoryEventFeed(log logr.Logger) EventFeed {
	return &inMemoryFeed{
		log:   log,
		repos: make(map[string]*repoState),
	}
}

// setNow overrides the feed's clock. Intended for tests only.
func (f *inMemoryFeed) setNow(fn func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = fn
}

func (f *inMemoryFeed) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// getOrCreate returns the repoState for repo, creating it if absent. Callers
// must hold f.mu for writing.
func (f *inMemoryFeed) getOrCreate(repo string) *repoState {
	rs, ok := f.repos[repo]
	if !ok {
		rs = &repoState{perLabelSet: make(map[string]int)}
		f.repos[repo] = rs
	}
	return rs
}

// RecordPush records a push event's head SHA and timestamp for repo. It does
// not fetch or parse workflow YAML; per-label-set counts come from
// RecordJob alone.
func (f *inMemoryFeed) RecordPush(repo, headSHA string) {
	if repo == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := f.getOrCreate(repo)
	rs.lastEvent = f.clock()
	rs.lastHeadSHA = headSHA
}

// RecordJob increments the per-label-set counter for repo using labels as
// deterministic ground truth from the webhook payload.
func (f *inMemoryFeed) RecordJob(repo string, labels []string) {
	if repo == "" || labels == nil {
		return
	}
	key := predictor.LabelSetKey(labels)

	f.mu.Lock()
	defer f.mu.Unlock()
	rs := f.getOrCreate(repo)
	rs.perLabelSet[key]++
	rs.lastEvent = f.clock()
}

// Snapshot returns a defensive copy of repo's per-label-set counts and its
// most recent event timestamp. It returns (nil, time.Time{}) when repo has
// no recorded events.
func (f *inMemoryFeed) Snapshot(repo string) (map[string]int, time.Time) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	rs, ok := f.repos[repo]
	if !ok {
		return nil, time.Time{}
	}

	perLabelSet := make(map[string]int, len(rs.perLabelSet))
	for k, v := range rs.perLabelSet {
		perLabelSet[k] = v
	}
	return perLabelSet, rs.lastEvent
}
