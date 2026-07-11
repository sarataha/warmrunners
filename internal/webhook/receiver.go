package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/sarataha/warmrunners/api/v1alpha1"
)

// AppLookup resolves the GitHubApp and webhook secret for the installation
// target that delivered a webhook, identified by the
// X-GitHub-Hook-Installation-Target-ID header value.
type AppLookup interface {
	Resolve(ctx context.Context, targetID string) (app *v1alpha1.GitHubApp, secret []byte, err error)
}

// Receiver is the HTTP handler for inbound GitHub webhook deliveries. It
// verifies the HMAC signature, guards against replayed deliveries, and
// dispatches verified events to the Dispatcher.
type Receiver struct {
	lookup AppLookup
	guard  *ReplayGuard
	disp   *Dispatcher
	log    logr.Logger
}

// NewReceiver constructs a Receiver.
func NewReceiver(lookup AppLookup, guard *ReplayGuard, disp *Dispatcher, log logr.Logger) *Receiver {
	return &Receiver{lookup: lookup, guard: guard, disp: disp, log: log}
}

// maxBodyBytes bounds the size of an accepted webhook delivery body.
const maxBodyBytes = 1 << 20

// ServeHTTP implements http.Handler, verifying and dispatching a single
// inbound GitHub webhook delivery. See the handler step order in the v0.5.0
// plan (Task 5) — each stage short-circuits on failure and records the
// corresponding metric.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxBodyBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			r.drop(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		r.drop(w, http.StatusBadRequest, "malformed")
		return
	}

	eventType := req.Header.Get("X-GitHub-Event")
	deliveryID := req.Header.Get("X-GitHub-Delivery")
	sigHeader := req.Header.Get("X-Hub-Signature-256")
	targetID := req.Header.Get("X-GitHub-Hook-Installation-Target-ID")
	if eventType == "" || deliveryID == "" || sigHeader == "" || targetID == "" {
		r.drop(w, http.StatusBadRequest, "malformed")
		return
	}

	ctx := req.Context()

	status, err := r.handle(ctx, eventType, deliveryID, sigHeader, targetID, body, start)
	if err != nil {
		r.drop(w, status, dropReasonForStatus(status))
		return
	}
	w.WriteHeader(status)
}

// handle runs the lookup -> HMAC verify -> replay guard -> dispatch pipeline
// shared by ServeHTTP (the HTTP path) and HandleFrame (the tunnel path). It
// returns the HTTP-style status code that describes the outcome (used by
// ServeHTTP; ignored by HandleFrame) and a non-nil error whenever the
// delivery was not accepted.
func (r *Receiver) handle(ctx context.Context, eventType, deliveryID, sigHeader, targetID string, body []byte, start time.Time) (int, error) {
	_, secret, err := r.lookup.Resolve(ctx, targetID)
	if err != nil {
		return http.StatusNotFound, fmt.Errorf("webhook: unknown installation target: %w", err)
	}

	if err := VerifySignature(body, sigHeader, secret); err != nil {
		return http.StatusUnauthorized, fmt.Errorf("webhook: invalid signature: %w", err)
	}

	if r.guard.Seen(deliveryID) {
		DeliveriesDropped.WithLabelValues("replay").Inc()
		return http.StatusOK, nil
	}

	if err := r.disp.Handle(ctx, eventType, deliveryID, body); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("webhook: dispatch: %w", err)
	}

	EventsTotal.WithLabelValues(eventType, "true", "").Inc()
	if eventType != "ping" {
		LagSeconds.WithLabelValues(eventType).Observe(time.Since(start).Seconds())
	}
	return http.StatusOK, nil
}

// HandleFrame runs a decoded relay frame (from a tunnel client) through the
// same HMAC verify + replay guard + dispatch pipeline as ServeHTTP. It
// satisfies FrameHandler. headers keys are the same GitHub webhook header
// names used over HTTP (X-GitHub-Event, X-GitHub-Delivery,
// X-Hub-Signature-256, X-GitHub-Hook-Installation-Target-ID).
func (r *Receiver) HandleFrame(ctx context.Context, headers map[string]string, body []byte) error {
	eventType := headers["X-GitHub-Event"]
	deliveryID := headers["X-GitHub-Delivery"]
	sigHeader := headers["X-Hub-Signature-256"]
	targetID := headers["X-GitHub-Hook-Installation-Target-ID"]
	if eventType == "" || deliveryID == "" || sigHeader == "" || targetID == "" {
		return errors.New("webhook: malformed tunnel frame: missing required header")
	}

	_, err := r.handle(ctx, eventType, deliveryID, sigHeader, targetID, body, time.Now())
	return err
}

// drop records a dropped-delivery metric and writes the response status.
func (r *Receiver) drop(w http.ResponseWriter, status int, reason string) {
	DeliveriesDropped.WithLabelValues(reason).Inc()
	w.WriteHeader(status)
}

// dropReasonForStatus maps a handle() failure status code back to the
// DeliveriesDropped reason label used by ServeHTTP's metrics.
func dropReasonForStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return "unknown_app"
	case http.StatusUnauthorized:
		return "hmac_invalid"
	default:
		return "malformed"
	}
}
