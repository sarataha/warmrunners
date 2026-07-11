package webhook

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
)

// eventFeed is the local view of the activity event feed used by the
// dispatcher. It is unexported so that PR2 can define the exported
// interface in the activity package and this file can adopt it without
// changing callers in this package.
type eventFeed interface {
	RecordPush(repo, headSHA string)
	RecordJob(repo string, labels []string)
}

// Parser is a placeholder for the per-SHA fanout refresh wiring that lands
// in PR2 when the WRP controller is reconciled. For now it is unused by
// Handle.
type Parser any

// pushPayload holds the fields consumed from a GitHub "push" webhook
// delivery.
type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// workflowJobPayload holds the fields consumed from a GitHub
// "workflow_job" webhook delivery.
type workflowJobPayload struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		Labels []string `json:"labels"`
		RunID  int64    `json:"run_id"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Dispatcher translates verified GitHub webhook deliveries into calls on
// the activity event feed.
type Dispatcher struct {
	feed   eventFeed
	parser Parser
	log    logr.Logger
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(feed eventFeed, parser Parser, log logr.Logger) *Dispatcher {
	return &Dispatcher{feed: feed, parser: parser, log: log}
}

// Handle processes a single webhook delivery identified by its GitHub
// event type and delivery ID. body is the raw (already HMAC-verified)
// request body.
func (d *Dispatcher) Handle(ctx context.Context, eventType string, deliveryID string, body []byte) error {
	switch eventType {
	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("webhook: decode push payload: %w", err)
		}
		d.feed.RecordPush(p.Repository.FullName, p.After)
		return nil
	case "workflow_job":
		var p workflowJobPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("webhook: decode workflow_job payload: %w", err)
		}
		if p.Action != "queued" {
			return nil
		}
		d.feed.RecordJob(p.Repository.FullName, p.WorkflowJob.Labels)
		return nil
	case "ping":
		return nil
	default:
		return nil
	}
}
