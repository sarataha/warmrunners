package webhook

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
)

// fakeFeed records calls made by the dispatcher for assertions in tests.
// It is not the real activity event feed; PR2 formalises that interface.
type fakeFeed struct {
	pushes []pushCall
	jobs   []jobCall
}

type pushCall struct {
	repo    string
	headSHA string
}

type jobCall struct {
	repo   string
	labels []string
}

func (f *fakeFeed) RecordPush(repo, headSHA string) {
	f.pushes = append(f.pushes, pushCall{repo: repo, headSHA: headSHA})
}

func (f *fakeFeed) RecordJob(repo string, labels []string) {
	f.jobs = append(f.jobs, jobCall{repo: repo, labels: labels})
}

func TestDispatcher_PushExtendsActivity(t *testing.T) {
	feed := &fakeFeed{}
	d := NewDispatcher(feed, nil, logr.Discard())

	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "deadbeefcafef00d",
		"repository": {"full_name": "acme/widgets"}
	}`)

	if err := d.Handle(context.Background(), "push", "delivery-1", body); err != nil {
		t.Fatalf("Handle() returned error: %v", err)
	}

	if len(feed.pushes) != 1 {
		t.Fatalf("expected 1 RecordPush call, got %d", len(feed.pushes))
	}
	got := feed.pushes[0]
	if got.repo != "acme/widgets" || got.headSHA != "deadbeefcafef00d" {
		t.Errorf("RecordPush call = %+v, want repo=acme/widgets headSHA=deadbeefcafef00d", got)
	}
}

func TestDispatcher_WorkflowJobQueued(t *testing.T) {
	feed := &fakeFeed{}
	d := NewDispatcher(feed, nil, logr.Discard())

	body := []byte(`{
		"action": "queued",
		"workflow_job": {"labels": ["self-hosted", "linux"], "run_id": 123},
		"repository": {"full_name": "acme/widgets"}
	}`)

	if err := d.Handle(context.Background(), "workflow_job", "delivery-2", body); err != nil {
		t.Fatalf("Handle() returned error: %v", err)
	}

	if len(feed.jobs) != 1 {
		t.Fatalf("expected 1 RecordJob call, got %d", len(feed.jobs))
	}
	got := feed.jobs[0]
	if got.repo != "acme/widgets" {
		t.Errorf("RecordJob repo = %q, want acme/widgets", got.repo)
	}
	if len(got.labels) != 2 || got.labels[0] != "self-hosted" || got.labels[1] != "linux" {
		t.Errorf("RecordJob labels = %v, want [self-hosted linux]", got.labels)
	}
}

func TestDispatcher_WorkflowJobNonQueuedIgnored(t *testing.T) {
	feed := &fakeFeed{}
	d := NewDispatcher(feed, nil, logr.Discard())

	body := []byte(`{
		"action": "completed",
		"workflow_job": {"labels": ["self-hosted"], "run_id": 456},
		"repository": {"full_name": "acme/widgets"}
	}`)

	if err := d.Handle(context.Background(), "workflow_job", "delivery-3", body); err != nil {
		t.Fatalf("Handle() returned error: %v", err)
	}

	if len(feed.jobs) != 0 {
		t.Errorf("expected no RecordJob calls for non-queued action, got %d", len(feed.jobs))
	}
}

func TestDispatcher_UnknownEventNoop(t *testing.T) {
	feed := &fakeFeed{}
	d := NewDispatcher(feed, nil, logr.Discard())

	body := []byte(`{"zen": "Responsive is better than fast."}`)

	if err := d.Handle(context.Background(), "ping", "delivery-4", body); err != nil {
		t.Fatalf("Handle() returned error for ping: %v", err)
	}
	if err := d.Handle(context.Background(), "issue_comment", "delivery-5", body); err != nil {
		t.Fatalf("Handle() returned error for unknown event: %v", err)
	}

	if len(feed.pushes) != 0 || len(feed.jobs) != 0 {
		t.Errorf("expected no feed calls for ping/unknown events, got pushes=%d jobs=%d", len(feed.pushes), len(feed.jobs))
	}
}

func TestDispatcher_MalformedBodyReturnsError(t *testing.T) {
	feed := &fakeFeed{}
	d := NewDispatcher(feed, nil, logr.Discard())

	body := []byte(`{not valid json`)

	if err := d.Handle(context.Background(), "push", "delivery-6", body); err == nil {
		t.Fatal("Handle() returned nil error for malformed JSON, want error")
	}
}
