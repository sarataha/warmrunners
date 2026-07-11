package webhook

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sarataha/warmrunners/api/v1alpha1"
)

// fakeLookup is a minimal AppLookup test double.
type fakeLookup struct {
	app    *v1alpha1.GitHubApp
	secret []byte
	err    error
}

func (f *fakeLookup) Resolve(ctx context.Context, targetID string) (*v1alpha1.GitHubApp, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.app, f.secret, nil
}

func newTestReceiver(lookup AppLookup, feed *fakeFeed) *Receiver {
	guard := NewReplayGuard(1024, time.Hour)
	disp := NewDispatcher(feed, nil, logr.Discard())
	return NewReceiver(lookup, guard, disp, logr.Discard())
}

func TestReceiver_HappyPath(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"acme/widgets"}}`)
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "delivery-happy")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "12345")

	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(feed.pushes) != 1 {
		t.Fatalf("expected 1 dispatched push, got %d", len(feed.pushes))
	}
}

func TestReceiver_MissingHeaders(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{}`)
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	// Deliberately omit all GitHub headers.
	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestReceiver_InvalidSignature(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"acme/widgets"}}`)
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "delivery-bad-sig")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(make([]byte, 32)))
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "12345")

	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(feed.pushes) != 0 {
		t.Errorf("expected no dispatch on invalid signature, got %d", len(feed.pushes))
	}
}

func TestReceiver_BodyTooLarge(t *testing.T) {
	secret := []byte("s3cr3t")
	body := bytes.Repeat([]byte("a"), (1<<20)+1)
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "delivery-large")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "12345")

	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

func TestReceiver_Replay(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"acme/widgets"}}`)
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", "delivery-replay")
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "12345")
		return req
	}

	w1 := httptest.NewRecorder()
	recv.ServeHTTP(w1, makeReq())
	if w1.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200", w1.Code)
	}
	if len(feed.pushes) != 1 {
		t.Fatalf("expected 1 dispatch after first delivery, got %d", len(feed.pushes))
	}

	w2 := httptest.NewRecorder()
	recv.ServeHTTP(w2, makeReq())
	if w2.Code != http.StatusOK {
		t.Fatalf("replayed delivery status = %d, want 200", w2.Code)
	}
	if len(feed.pushes) != 1 {
		t.Fatalf("expected dispatcher NOT called again on replay, still want 1, got %d", len(feed.pushes))
	}
}

func TestReceiver_UnknownInstallationTarget(t *testing.T) {
	lookup := &fakeLookup{err: errors.New("app not found")}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "delivery-unknown")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "99999")

	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestReceiver_HandleFrame(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"ref":"refs/heads/main","after":"deadbeef","repository":{"full_name":"acme/widgets"}}`)

	t.Run("unknown target", func(t *testing.T) {
		lookup := &fakeLookup{err: errors.New("app not found")}
		feed := &fakeFeed{}
		recv := newTestReceiver(lookup, feed)

		headers := map[string]string{
			"X-GitHub-Event":                       "push",
			"X-GitHub-Delivery":                    "frame-unknown",
			"X-Hub-Signature-256":                  sign(secret, body),
			"X-GitHub-Hook-Installation-Target-ID": "99999",
		}
		if err := recv.HandleFrame(context.Background(), headers, body); err == nil {
			t.Fatal("expected error for unknown installation target, got nil")
		}
	})

	t.Run("HMAC is intentionally not verified (tunnel = trusted-relay)", func(t *testing.T) {
		lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
		feed := &fakeFeed{}
		recv := newTestReceiver(lookup, feed)

		// Deliberately bogus signature — smee.io reserialises the JSON body,
		// so GitHub's signature would never match the bytes we see. Tunnel
		// mode dispatches regardless; ServeHTTP still enforces HMAC.
		headers := map[string]string{
			"X-GitHub-Event":                       "push",
			"X-GitHub-Delivery":                    "frame-tunnel-sig",
			"X-Hub-Signature-256":                  "sha256=" + hex.EncodeToString(make([]byte, 32)),
			"X-GitHub-Hook-Installation-Target-ID": "12345",
		}
		if err := recv.HandleFrame(context.Background(), headers, body); err != nil {
			t.Fatalf("expected nil error for tunnel frame, got %v", err)
		}
		if len(feed.pushes) != 1 {
			t.Errorf("expected dispatch on tunnel frame, got %d", len(feed.pushes))
		}
	})

	t.Run("replay is idempotent", func(t *testing.T) {
		lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
		feed := &fakeFeed{}
		recv := newTestReceiver(lookup, feed)

		headers := map[string]string{
			"X-GitHub-Event":                       "push",
			"X-GitHub-Delivery":                    "frame-replay",
			"X-Hub-Signature-256":                  sign(secret, body),
			"X-GitHub-Hook-Installation-Target-ID": "12345",
		}
		if err := recv.HandleFrame(context.Background(), headers, body); err != nil {
			t.Fatalf("first HandleFrame: unexpected error: %v", err)
		}
		if err := recv.HandleFrame(context.Background(), headers, body); err != nil {
			t.Fatalf("replayed HandleFrame: expected nil error, got: %v", err)
		}
		if len(feed.pushes) != 1 {
			t.Fatalf("expected 1 dispatch total (replay is idempotent), got %d", len(feed.pushes))
		}
	})

	t.Run("good frame dispatches and increments EventsTotal", func(t *testing.T) {
		lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: secret}
		feed := &fakeFeed{}
		recv := newTestReceiver(lookup, feed)

		before := testutil.ToFloat64(EventsTotal.WithLabelValues("push", "tunnel", ""))

		headers := map[string]string{
			"X-GitHub-Event":                       "push",
			"X-GitHub-Delivery":                    "frame-good",
			"X-Hub-Signature-256":                  sign(secret, body),
			"X-GitHub-Hook-Installation-Target-ID": "12345",
		}
		if err := recv.HandleFrame(context.Background(), headers, body); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(feed.pushes) != 1 {
			t.Fatalf("expected 1 dispatched push, got %d", len(feed.pushes))
		}

		after := testutil.ToFloat64(EventsTotal.WithLabelValues("push", "tunnel", ""))
		if after != before+1 {
			t.Fatalf("EventsTotal push/tunnel = %v, want %v", after, before+1)
		}
	})
}

func TestReceiver_NonPostMethod(t *testing.T) {
	lookup := &fakeLookup{app: &v1alpha1.GitHubApp{}, secret: []byte("s3cr3t")}
	feed := &fakeFeed{}
	recv := newTestReceiver(lookup, feed)

	req := httptest.NewRequest(http.MethodGet, "/github/webhook", nil)
	w := httptest.NewRecorder()
	recv.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}
