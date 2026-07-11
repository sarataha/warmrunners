package webhook

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"nhooyr.io/websocket"
)

// FrameHandler is what the tunnel client dispatches decoded relay frames
// into. The HTTP Receiver satisfies this interface, so tunnelled frames run
// through the same HMAC + replay + dispatch path as HTTP deliveries.
type FrameHandler interface {
	HandleFrame(ctx context.Context, headers map[string]string, body []byte) error
}

// TunnelClient maintains a long-lived WebSocket connection to a relay
// (smee.io-compatible) and delivers each relayed webhook frame to a
// FrameHandler.
type TunnelClient interface {
	// Start dials relayURL and delivers each event to the handler until
	// ctx is cancelled. Auto-reconnects with exponential backoff (500ms →
	// 30s cap, full jitter). Returns nil on ctx.Done(), error on
	// unrecoverable auth failures.
	Start(ctx context.Context, relayURL string) error
	// Connected reports the last-known connection state.
	Connected() bool
}

// frame is a single relay message, smee.io-compatible: the original GitHub
// webhook headers plus the raw JSON body.
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

	// maxBackoff overrides maxBackoffCap; tests set this to keep the
	// reconnect loop fast. Zero means "use maxBackoffCap".
	maxBackoff time.Duration

	mu        sync.RWMutex
	connected bool
}

// NewTunnelClient constructs a TunnelClient that dispatches decoded relay
// frames to handler.
func NewTunnelClient(handler FrameHandler, log logr.Logger) TunnelClient {
	return &tunnelClient{handler: handler, log: log}
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

func (c *tunnelClient) Start(ctx context.Context, relayURL string) error {
	backoff := initialBackoff

	for {
		select {
		case <-ctx.Done():
			c.setConnected(false)
			return nil
		default:
		}

		conn, _, err := websocket.Dial(ctx, relayURL, nil)
		if err != nil {
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
		c.setConnected(true)
		c.readLoop(ctx, conn)
		c.setConnected(false)
		conn.Close(websocket.StatusNormalClosure, "reconnect")

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

// readLoop reads and dispatches frames from conn until the connection
// errors (closed by peer, context cancelled, etc).
func (c *tunnelClient) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.log.Error(err, "tunnel: bad frame")
			continue
		}
		if err := c.handler.HandleFrame(ctx, f.headers(), f.Body); err != nil {
			c.log.Error(err, "tunnel: handle frame failed")
		}
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
		_ = tc.Start(ctx, relayURL)
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
