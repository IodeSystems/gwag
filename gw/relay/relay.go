package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// readLimit caps one client frame: control frames only, so a cap token is the
// largest thing in one.
const readLimit = 32 << 10

// inFrame is a client→relay control frame.
type inFrame struct {
	Op    string `json:"op"`    // "sub" | "unsub" | "ping"
	ID    string `json:"id"`    // client-chosen subscription id (multiplex key)
	Token string `json:"token"` // signed Cap (op "sub" only)
}

// outFrame is a relay→client frame.
type outFrame struct {
	Op      string          `json:"op"`                // "data" | "ack" | "err" | "pong"
	ID      string          `json:"id"`                // echoes the client sub id
	Payload json.RawMessage `json:"payload,omitempty"` // op "data": the raw bus payload
	Err     string          `json:"err,omitempty"`     // op "err"

	// prepared is the wire form of this frame shared with other sockets
	// (WithCompression); the fields above are unused when it is set.
	prepared *websocket.PreparedMessage
}

// Relay multiplexes many cap-gated channel subscriptions over one WebSocket.
// It is identity-agnostic: a client may subscribe to any channel it presents a
// valid Cap for, and the relay re-derives nothing about who the client is.
type Relay struct {
	secret   []byte
	src      Source
	now      func() time.Time
	queue    int
	writeTO  time.Duration
	coalesce bool
	compress bool
	frames   *frameCache // WithCompression only
}

// Option configures a Relay.
type Option func(*Relay)

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(r *Relay) { r.now = now } }

// WithQueue sets the per-connection outbound buffer. When full, the oldest-pending
// frame is dropped (the client refetches on the next event), never blocking the bus.
func WithQueue(n int) Option {
	return func(r *Relay) {
		if n > 0 {
			r.queue = n
		}
	}
}

// WithWriteTimeout caps how long a single frame write may take before teardown.
func WithWriteTimeout(d time.Duration) Option { return func(r *Relay) { r.writeTO = d } }

// WithCompression negotiates permessage-deflate with clients that offer it
// (browsers do), in no-context-takeover mode: frames of compressThreshold bytes
// or more are compressed, so there is no fixed per-connection cost. A data frame
// is compressed once per event per distinct subscription id on its channel and
// the result written to every socket that holds that id, so one event to N
// viewers costs one compression when they subscribed in the same order (the
// generated runtime numbers subs per page). Off by default.
func WithCompression() Option { return func(r *Relay) { r.compress = true } }

// WithoutCoalescing disables the default per-channel subscription coalescing, so
// each client subscription opens its own upstream subscription. Rarely wanted —
// only if a Source has per-subscriber semantics that must not be shared.
func WithoutCoalescing() Option { return func(r *Relay) { r.coalesce = false } }

// New builds a relay that verifies caps with secret and fans events from src.
// By default the source is wrapped in Coalesce, so the relay holds one upstream
// subscription per channel regardless of how many local clients watch it.
func New(secret []byte, src Source, opts ...Option) *Relay {
	r := &Relay{secret: secret, now: time.Now, queue: 64, writeTO: 5 * time.Second, coalesce: true}
	for _, o := range opts {
		o(r)
	}
	if r.coalesce {
		src = Coalesce(src)
	}
	r.src = src
	if r.compress {
		r.frames = newFrameCache()
	}
	return r
}

// Handler upgrades GET requests to a multiplexed subscription WebSocket.
// Connection-level concerns (origin allow-listing, cookie auth if any, rate
// limits) are the caller's to impose around this handler; the relay itself gates
// only per-subscription, by the signed cap. OriginPatterns is left open here —
// wrap with the caller's origin policy when serving cross-origin.
func (r *Relay) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		up := websocket.Upgrader{
			CheckOrigin:       func(*http.Request) bool { return true },
			EnableCompression: r.compress,
		}
		c, err := up.Upgrade(w, req, nil)
		if err != nil {
			return // Upgrade already wrote the failure
		}
		defer func() { _ = c.Close() }()
		r.serve(req.Context(), c)
	})
}

// serve runs one connection: a single writer goroutine drains the outbound
// buffer, while this goroutine reads control frames and (un)subscribes the
// Source. gorilla permits one concurrent reader + one concurrent writer, which is
// exactly this split.
func (r *Relay) serve(ctx context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.SetReadLimit(readLimit)
	// Reads take no context: closing the conn is what ends a blocked read.
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	out := make(chan outFrame, r.queue)

	var mu sync.Mutex
	subs := map[string]func(){} // sub id → Source cancel
	defer func() {
		mu.Lock()
		for _, cf := range subs {
			cf()
		}
		subs = map[string]func(){}
		mu.Unlock()
	}()

	// enqueue, non-blocking: drop the frame on overflow rather than stall the bus
	// goroutine. The client treats a gap as "refetch".
	send := func(f outFrame) {
		select {
		case out <- f:
		default:
		}
	}

	// writer
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-out:
				if err := r.write(c, f); err != nil {
					cancel() // client gone / write stalled — tear down
					return
				}
			}
		}
	}()

	for {
		var in inFrame
		if err := c.ReadJSON(&in); err != nil {
			return // client closed or sent a non-JSON frame
		}
		switch in.Op {
		case "sub":
			r.handleSub(in, &mu, subs, send)
		case "ping":
			// Application-level heartbeat: a client that must keep an idle socket alive
			// through a proxy idle timeout, and notice a dead one, sends ping and expects
			// pong. (Browsers cannot send WebSocket ping control frames.)
			send(outFrame{Op: "pong", ID: in.ID})
		case "unsub":
			mu.Lock()
			if cf := subs[in.ID]; cf != nil {
				cf()
				delete(subs, in.ID)
			}
			mu.Unlock()
		default:
			send(outFrame{Op: "err", ID: in.ID, Err: "unknown op"})
		}
	}
}

// write sends one frame under the write timeout.
func (r *Relay) write(c *websocket.Conn, f outFrame) error {
	if err := c.SetWriteDeadline(time.Now().Add(r.writeTO)); err != nil {
		return err
	}
	if f.prepared != nil {
		c.EnableWriteCompression(true)
		return c.WritePreparedMessage(f.prepared)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.EnableWriteCompression(len(b) >= compressThreshold)
	return c.WriteMessage(websocket.TextMessage, b)
}

func (r *Relay) handleSub(in inFrame, mu *sync.Mutex, subs map[string]func(), send func(outFrame)) {
	if in.ID == "" {
		send(outFrame{Op: "err", Err: "missing id"})
		return
	}
	cp, err := Verify(r.secret, in.Token, r.now())
	if err != nil {
		send(outFrame{Op: "err", ID: in.ID, Err: err.Error()})
		return
	}
	id := in.ID
	channel := cp.Channel
	r.frames.acquire(channel)
	cancel, err := r.src.Subscribe(channel, func(payload []byte) {
		f := outFrame{Op: "data", ID: id, Payload: json.RawMessage(payload)}
		f.prepared = r.frames.frame(channel, f)
		send(f)
	})
	if err != nil {
		r.frames.release(channel)
		send(outFrame{Op: "err", ID: id, Err: "subscribe failed"})
		return
	}
	mu.Lock()
	if old := subs[id]; old != nil {
		old() // a re-sub on the same id replaces the prior subscription
	}
	subs[id] = func() {
		cancel()
		r.frames.release(channel)
	}
	mu.Unlock()
	send(outFrame{Op: "ack", ID: id})
}
