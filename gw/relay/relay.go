package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// inFrame is a client→relay control frame.
type inFrame struct {
	Op    string `json:"op"`    // "sub" | "unsub"
	ID    string `json:"id"`    // client-chosen subscription id (multiplex key)
	Token string `json:"token"` // signed Cap (op "sub" only)
}

// outFrame is a relay→client frame.
type outFrame struct {
	Op      string          `json:"op"`                // "data" | "ack" | "err"
	ID      string          `json:"id"`                // echoes the client sub id
	Payload json.RawMessage `json:"payload,omitempty"` // op "data": the raw bus payload
	Err     string          `json:"err,omitempty"`     // op "err"
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
	return r
}

// Handler upgrades GET requests to a multiplexed subscription WebSocket.
// Connection-level concerns (origin allow-listing, cookie auth if any, rate
// limits) are the caller's to impose around this handler; the relay itself gates
// only per-subscription, by the signed cap. OriginPatterns is left open here —
// wrap with the caller's origin policy when serving cross-origin.
func (r *Relay) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return // Accept already wrote the failure
		}
		defer func() { _ = c.CloseNow() }()
		r.serve(req.Context(), c)
	})
}

// serve runs one connection: a single writer goroutine drains the outbound
// buffer, while this goroutine reads control frames and (un)subscribes the
// Source. nhooyr permits one concurrent reader + one concurrent writer, which is
// exactly this split.
func (r *Relay) serve(ctx context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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
				wctx, wcancel := context.WithTimeout(ctx, r.writeTO)
				err := wsjson.Write(wctx, c, f)
				wcancel()
				if err != nil {
					cancel() // client gone / write stalled — tear down
					return
				}
			}
		}
	}()

	for {
		var in inFrame
		if err := wsjson.Read(ctx, c, &in); err != nil {
			return // client closed or sent a non-JSON frame
		}
		switch in.Op {
		case "sub":
			r.handleSub(in, &mu, subs, send)
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
	cancel, err := r.src.Subscribe(cp.Channel, func(payload []byte) {
		send(outFrame{Op: "data", ID: id, Payload: json.RawMessage(payload)})
	})
	if err != nil {
		send(outFrame{Op: "err", ID: id, Err: "subscribe failed"})
		return
	}
	mu.Lock()
	if old := subs[id]; old != nil {
		old() // a re-sub on the same id replaces the prior subscription
	}
	subs[id] = cancel
	mu.Unlock()
	send(outFrame{Op: "ack", ID: id})
}
