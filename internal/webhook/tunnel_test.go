package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"nhooyr.io/websocket"
)

// fakeHandler is a test FrameHandler that records every call.
type fakeHandler struct {
	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	Headers map[string]string
	Body    []byte
}

func (h *fakeHandler) HandleFrame(_ context.Context, headers map[string]string, body []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := make(map[string]string, len(headers))
	for k, v := range headers {
		hs[k] = v
	}
	h.calls = append(h.calls, fakeCall{Headers: hs, Body: append([]byte(nil), body...)})
	return nil
}

func (h *fakeHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *fakeHandler) call(i int) fakeCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[i]
}

// waitFor polls cond every 5ms until it returns true or timeout elapses. It
// reports a fatal error if the timeout is reached first.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// sendFrame encodes a relay frame and writes it as a text message on conn.
func sendFrame(ctx context.Context, conn *websocket.Conn, event, delivery, sig, targetID string, body json.RawMessage) error {
	data, err := json.Marshal(map[string]any{
		"x-github-event":                       event,
		"x-github-delivery":                    delivery,
		"x-hub-signature-256":                  sig,
		"x-github-hook-installation-target-id": targetID,
		"body":                                 body,
	})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func TestTunnelClient_HandleFrameCalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = sendFrame(r.Context(), conn, "push", "d1", "sig1", "123", json.RawMessage(`{"k":"v"}`))
		// Keep the connection open briefly so the client can read the frame.
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]

	handler := &fakeHandler{}
	tc := NewTunnelClient(handler, logr.Discard())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = tc.Start(ctx, wsURL) }()

	waitFor(t, func() bool { return handler.count() >= 1 }, time.Second)

	call := handler.call(0)
	if call.Headers["X-GitHub-Event"] != "push" {
		t.Errorf("expected X-GitHub-Event=push, got %q", call.Headers["X-GitHub-Event"])
	}
	if call.Headers["X-GitHub-Delivery"] != "d1" {
		t.Errorf("expected X-GitHub-Delivery=d1, got %q", call.Headers["X-GitHub-Delivery"])
	}
	if call.Headers["X-Hub-Signature-256"] != "sig1" {
		t.Errorf("expected X-Hub-Signature-256=sig1, got %q", call.Headers["X-Hub-Signature-256"])
	}
	if call.Headers["X-GitHub-Hook-Installation-Target-ID"] != "123" {
		t.Errorf("expected target id=123, got %q", call.Headers["X-GitHub-Hook-Installation-Target-ID"])
	}
	if string(call.Body) != `{"k":"v"}` {
		t.Errorf("expected body {\"k\":\"v\"}, got %q", call.Body)
	}
}

func TestTunnelClient_ReconnectsOnServerClose(t *testing.T) {
	var mu sync.Mutex
	connCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		connCount++
		n := connCount
		mu.Unlock()

		if n == 1 {
			// First connection: send one frame, then close abruptly to force
			// a reconnect.
			_ = sendFrame(r.Context(), conn, "push", "d1", "sig1", "123", json.RawMessage(`{"n":1}`))
			time.Sleep(20 * time.Millisecond)
			conn.Close(websocket.StatusNormalClosure, "bye")
			return
		}

		// Second (reconnected) connection: send a second frame and hold
		// briefly.
		defer conn.Close(websocket.StatusNormalClosure, "")
		_ = sendFrame(r.Context(), conn, "push", "d2", "sig2", "123", json.RawMessage(`{"n":2}`))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]

	handler := &fakeHandler{}
	tc := NewTunnelClient(handler, logr.Discard()).(*tunnelClient)
	tc.maxBackoff = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = tc.Start(ctx, wsURL) }()

	waitFor(t, func() bool { return handler.count() >= 2 }, 2*time.Second)

	if handler.call(0).Headers["X-GitHub-Delivery"] != "d1" {
		t.Errorf("expected first delivery d1, got %q", handler.call(0).Headers["X-GitHub-Delivery"])
	}
	if handler.call(1).Headers["X-GitHub-Delivery"] != "d2" {
		t.Errorf("expected second delivery d2, got %q", handler.call(1).Headers["X-GitHub-Delivery"])
	}
}

func TestTunnelClient_ConnectedTransitions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		<-r.Context().Done()
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]

	handler := &fakeHandler{}
	tc := NewTunnelClient(handler, logr.Discard())

	if tc.Connected() {
		t.Fatalf("expected Connected()==false before Start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = tc.Start(ctx, wsURL) }()

	waitFor(t, tc.Connected, time.Second)

	cancel()

	waitFor(t, func() bool { return !tc.Connected() }, time.Second)
}

func TestTunnelRegistry_EnsureIdempotent(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	factory := func() TunnelClient {
		mu.Lock()
		calls++
		mu.Unlock()
		return &noopTunnelClient{}
	}

	reg := NewTunnelRegistry(factory)
	defer reg.Stop("app1")

	c1 := reg.Ensure("app1", "wss://ignored")
	c2 := reg.Ensure("app1", "wss://ignored")

	if c1 != c2 {
		t.Errorf("expected same instance from repeated Ensure with same URL")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("expected factory called once, got %d", calls)
	}
}

func TestTunnelRegistry_EnsureReplacesOnURLChange(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	factory := func() TunnelClient {
		mu.Lock()
		calls++
		mu.Unlock()
		return &noopTunnelClient{}
	}

	reg := NewTunnelRegistry(factory)
	defer reg.Stop("app1")

	c1 := reg.Ensure("app1", "wss://a")
	c2 := reg.Ensure("app1", "wss://b")

	if c1 == c2 {
		t.Errorf("expected different instance after URL change")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("expected factory called twice, got %d", calls)
	}
}

func TestTunnelRegistry_Stop(t *testing.T) {
	factory := func() TunnelClient { return &noopTunnelClient{} }

	reg := NewTunnelRegistry(factory)
	reg.Ensure("app1", "wss://a")
	reg.Stop("app1")

	if got := reg.Get("app1"); got != nil {
		t.Errorf("expected Get to return nil after Stop, got %v", got)
	}
}

// noopTunnelClient is a minimal TunnelClient used by registry tests, where
// no real connection is ever attempted.
type noopTunnelClient struct {
	mu        sync.Mutex
	connected bool
}

func (n *noopTunnelClient) Start(ctx context.Context, _ string) error {
	n.mu.Lock()
	n.connected = true
	n.mu.Unlock()
	<-ctx.Done()
	n.mu.Lock()
	n.connected = false
	n.mu.Unlock()
	return nil
}

func (n *noopTunnelClient) Connected() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.connected
}
