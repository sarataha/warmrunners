package webhook

import (
	"context"
	"errors"
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

	_, secret, err := r.lookup.Resolve(ctx, targetID)
	if err != nil {
		r.drop(w, http.StatusNotFound, "unknown_app")
		return
	}

	if err := VerifySignature(body, sigHeader, secret); err != nil {
		r.drop(w, http.StatusUnauthorized, "hmac_invalid")
		return
	}

	if r.guard.Seen(deliveryID) {
		DeliveriesDropped.WithLabelValues("replay").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := r.disp.Handle(ctx, eventType, deliveryID, body); err != nil {
		r.drop(w, http.StatusInternalServerError, "malformed")
		return
	}

	EventsTotal.WithLabelValues(eventType, "true", "").Inc()
	if eventType != "ping" {
		LagSeconds.WithLabelValues(eventType).Observe(time.Since(start).Seconds())
	}
	w.WriteHeader(http.StatusOK)
}

// drop records a dropped-delivery metric and writes the response status.
func (r *Receiver) drop(w http.ResponseWriter, status int, reason string) {
	DeliveriesDropped.WithLabelValues(reason).Inc()
	w.WriteHeader(status)
}
