package webhook

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// FrameHandler is what the tunnel client dispatches decoded relay frames
// into. The HTTP Receiver satisfies this interface, so tunnelled frames run
// through the same HMAC + replay + dispatch path as HTTP deliveries.
type FrameHandler interface {
	HandleFrame(ctx context.Context, headers map[string]string, body []byte) error
}

// TunnelClient maintains a long-lived Server-Sent Events connection to a
// relay (smee.io-compatible) and delivers each relayed webhook frame to a
// FrameHandler.
type TunnelClient interface {
	// Start dials relayURL and delivers each event to the handler until
	// ctx is cancelled. Auto-reconnects with exponential backoff (500ms →
	// 30s cap, full jitter). Returns nil on ctx.Done(). name identifies
	// the owning GitHubApp CR, used to label the
	// warmrunners_tunnel_reconnects_total counter.
	Start(ctx context.Context, name, relayURL string) error
	// Connected reports the last-known connection state.
	Connected() bool
}

// frame is the JSON payload smee.io sends inside the `data:` line of each
// SSE event. Keys match the outgoing GitHub webhook headers plus the raw
// request body.
type frame struct {
	Event     string          `json:"x-github-event"`
	Delivery  string          `json:"x-github-delivery"`
	Signature string          `json:"x-hub-signature-256"`
	TargetID  string          `json:"x-github-hook-installation-target-id"`
	Body      json.RawMessage `json:"body"`
}

// headers builds the GitHub-style header map passed to FrameHandler.HandleFrame.
func (f frame) headers() map[string]string {
	return map[string]string{
		"X-GitHub-Event":                       f.Event,
		"X-GitHub-Delivery":                    f.Delivery,
		"X-Hub-Signature-256":                  f.Signature,
		"X-GitHub-Hook-Installation-Target-ID": f.TargetID,
	}
}

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoffCap  = 30 * time.Second
)

// tunnelClient is the default TunnelClient implementation.
type tunnelClient struct {
	handler FrameHandler
	log     logr.Logger

	// httpClient is overridable in tests. Zero means DefaultTransport with
	// no timeout — SSE streams are meant to stay open indefinitely.
	httpClient *http.Client

	// maxBackoff overrides maxBackoffCap; tests set this to keep the
	// reconnect loop fast. Zero means "use maxBackoffCap".
	maxBackoff time.Duration

	mu        sync.RWMutex
	connected bool
}

// NewTunnelClient constructs a TunnelClient that dispatches decoded relay
// frames to handler.
func NewTunnelClient(handler FrameHandler, log logr.Logger) TunnelClient {
	return &tunnelClient{
		handler:    handler,
		log:        log,
		httpClient: &http.Client{},
	}
}

func (c *tunnelClient) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *tunnelClient) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

func (c *tunnelClient) cap() time.Duration {
	if c.maxBackoff > 0 {
		return c.maxBackoff
	}
	return maxBackoffCap
}

// Start opens an SSE connection to relayURL and dispatches every non-ready
// event through the handler. Reconnects on any error until ctx is cancelled.
func (c *tunnelClient) Start(ctx context.Context, name, relayURL string) error {
	backoff := initialBackoff

	for {
		select {
		case <-ctx.Done():
			c.setConnected(false)
			return nil
		default:
		}

		if err := c.streamOnce(ctx, name, relayURL); err != nil {
			if ctx.Err() != nil {
				c.setConnected(false)
				return nil
			}
			c.log.Info("tunnel dial failed", "url", relayURL, "error", err.Error(), "backoff", backoff)
			sleep := fullJitter(minDuration(backoff, c.cap()))
			select {
			case <-ctx.Done():
				c.setConnected(false)
				return nil
			case <-time.After(sleep):
			}
			backoff = minDuration(backoff*2, c.cap())
			continue
		}
		backoff = initialBackoff
	}
}

// streamOnce opens exactly one SSE stream and returns when it ends (peer
// close, ctx cancel, read error). Returns nil on ctx cancel, error otherwise.
func (c *tunnelClient) streamOnce(ctx context.Context, name, relayURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relayURL, nil)
	if err != nil {
		TunnelReconnects.WithLabelValues(name, "failure").Inc()
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		TunnelReconnects.WithLabelValues(name, "failure").Inc()
		return fmt.Errorf("http dial: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		TunnelReconnects.WithLabelValues(name, "failure").Inc()
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	TunnelReconnects.WithLabelValues(name, "success").Inc()

	c.setConnected(true)
	defer c.setConnected(false)

	return c.readSSE(ctx, resp.Body)
}

// readSSE parses `event:` / `data:` framed SSE messages from r and hands
// each parsed frame to the handler. Returns when r EOFs, errors, or ctx is
// cancelled.
func (c *tunnelClient) readSSE(ctx context.Context, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// smee.io payloads regularly exceed the default 64KiB buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var eventName string
	var dataBuf strings.Builder

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := scanner.Text()

		if line == "" {
			// Blank line: dispatch the accumulated event, if any.
			if dataBuf.Len() > 0 {
				c.dispatch(ctx, eventName, dataBuf.String())
			}
			eventName = ""
			dataBuf.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			// Comment line (smee sends `:ping` keepalives). Ignore.
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
		}
	}
	return scanner.Err()
}

// dispatch decodes one SSE data payload as a smee frame and hands it to the
// handler. `ready` events (sent once on connect) carry no webhook payload
// and are skipped.
func (c *tunnelClient) dispatch(ctx context.Context, eventName, data string) {
	if eventName == "ready" {
		return
	}
	var f frame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		c.log.Error(err, "tunnel: bad frame")
		return
	}
	if f.Event == "" && f.Delivery == "" && f.Signature == "" {
		// smee sometimes wraps events in `{"…":{…}}`; nothing useful.
		return
	}
	if err := c.handler.HandleFrame(ctx, f.headers(), f.Body); err != nil {
		c.log.Error(err, "tunnel: handle frame failed")
	}
}

// fullJitter returns a random duration in [0, d).
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// tunnelEntry tracks a running tunnel and how to stop it.
type tunnelEntry struct {
	client TunnelClient
	url    string
	cancel context.CancelFunc
	done   chan struct{}
}

// TunnelRegistry manages one running TunnelClient per GitHubApp CR, keyed
// by GitHubApp name.
type TunnelRegistry struct {
	factory func() TunnelClient

	mu      sync.Mutex
	entries map[string]*tunnelEntry
}

// NewTunnelRegistry constructs a TunnelRegistry. factory is called to
// create a fresh TunnelClient each time a tunnel is (re)started.
func NewTunnelRegistry(factory func() TunnelClient) *TunnelRegistry {
	return &TunnelRegistry{
		factory: factory,
		entries: make(map[string]*tunnelEntry),
	}
}

// Ensure makes sure a tunnel is running for name at relayURL. If one is
// already running at the same URL, it is left untouched and returned. If
// one is running at a different URL, it is stopped and replaced.
func (r *TunnelRegistry) Ensure(name, relayURL string) TunnelClient {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.entries[name]; ok {
		if existing.url == relayURL {
			return existing.client
		}
		r.stopLocked(name)
	}

	return r.startLocked(name, relayURL)
}

// startLocked creates and starts a new tunnel for name. Caller must hold r.mu.
func (r *TunnelRegistry) startLocked(name, relayURL string) TunnelClient {
	tc := r.factory()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tc.Start(ctx, name, relayURL)
	}()
	r.entries[name] = &tunnelEntry{client: tc, url: relayURL, cancel: cancel, done: done}
	return tc
}

// Stop cancels and removes the tunnel running for name, if any.
func (r *TunnelRegistry) Stop(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(name)
}

// stopLocked cancels the tunnel's context, waits briefly for its goroutine
// to exit, and removes it from the map. Caller must hold r.mu.
func (r *TunnelRegistry) stopLocked(name string) {
	entry, ok := r.entries[name]
	if !ok {
		return
	}
	entry.cancel()
	select {
	case <-entry.done:
	case <-time.After(2 * time.Second):
	}
	delete(r.entries, name)
}

// Get returns the tunnel running for name, or nil if none is present.
func (r *TunnelRegistry) Get(name string) TunnelClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[name]
	if !ok {
		return nil
	}
	return entry.client
}
