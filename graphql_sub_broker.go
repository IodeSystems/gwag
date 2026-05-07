package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// graphQLSubBroker shares one upstream graphql-transport-ws connection
// across N local subscribers of the same downstream-GraphQL source.
// Same (query, variables) share an upstream subscription too — analogous
// to broker.go fanning out one NATS sub to N WebSocket consumers.
//
// Lifecycle: lazy-dialed on first acquire; closed when the last
// consumer leaves. A subsequent acquire re-dials a fresh connection.
//
// Concurrency model: broker.mu guards (conn, fanouts). Writes to conn
// are serialized via writeMu (websocket.Conn isn't safe for concurrent
// writes); reads come from one reader goroutine.
type graphQLSubBroker struct {
	src     *graphQLSource
	metrics Metrics

	mu      sync.Mutex
	conn    *websocket.Conn
	writeMu sync.Mutex
	fanouts map[string]*graphQLSubFanout // key → fanout
	closing bool                         // teardown in progress
}

// graphQLSubFanout is one upstream subscription whose `next` payloads
// fan out to many local subscribers. Each target is a buffered chan
// receiving *upstreamSubFrame; deliveries are non-blocking so a slow
// consumer drops events rather than gating the broker. Same drop
// policy as broker.go (see plan tier-3 "Sub-fanout drop policy
// configurable" if this becomes operationally meaningful).
type graphQLSubFanout struct {
	subID  string
	key    string
	broker *graphQLSubBroker

	mu       sync.Mutex
	nextID   uint64
	targets  map[uint64]chan *upstreamSubFrame
	terminal bool // upstream sent complete/error or conn died
}

func newGraphQLSubBroker(src *graphQLSource) *graphQLSubBroker {
	m := src.metrics
	if m == nil {
		m = noopMetrics{}
	}
	return &graphQLSubBroker{
		src:     src,
		metrics: m,
		fanouts: map[string]*graphQLSubFanout{},
	}
}

// recordOpenLocked emits an "open" event + active gauge. Caller holds b.mu.
func (b *graphQLSubBroker) recordOpenLocked() {
	b.metrics.RecordGraphQLSubFanout(b.src.namespace, "open")
	b.metrics.SetGraphQLSubFanoutsActive(b.src.namespace, len(b.fanouts))
}

// recordClosesLocked emits n "close" events + active gauge. Caller holds b.mu.
func (b *graphQLSubBroker) recordClosesLocked(n int) {
	for i := 0; i < n; i++ {
		b.metrics.RecordGraphQLSubFanout(b.src.namespace, "close")
	}
	b.metrics.SetGraphQLSubFanoutsActive(b.src.namespace, len(b.fanouts))
}

// acquire registers a target on the fanout for (query, variables),
// dialing the upstream WS lazily and sending the subscribe upstream
// the first time the key appears. Returns a channel of frames + a
// release func; release must be called exactly once per acquire.
//
// `endpoint` is captured per-acquire (replicas can differ across
// fanouts) but the WS connection is reused across fanouts when they
// share the same endpoint — first acquire wins. Mixed endpoints
// across calls re-dial.
//
// Headers are pulled from the request context via the same
// ForwardHeaders allowlist queries use, captured at the first acquire
// only — later acquires reuse the existing connection's auth.
func (b *graphQLSubBroker) acquire(
	ctx context.Context,
	endpoint string,
	query string,
	variables map[string]any,
	forwardHeaders []string,
) (<-chan *upstreamSubFrame, func(), error) {
	key := fanoutKey(query, variables)

	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("graphql ingest broker: closing")
	}
	if b.conn == nil {
		// Lazy dial + handshake. Hold broker.mu so concurrent acquires
		// queue behind the dial.
		conn, err := b.dialAndInitLocked(ctx, endpoint, forwardHeaders)
		if err != nil {
			b.mu.Unlock()
			return nil, nil, err
		}
		b.conn = conn
		go b.readerLoop(conn)
	}

	f, ok := b.fanouts[key]
	if !ok {
		// Send subscribe upstream — first local consumer for this op.
		subID := newWSSubID()
		subPayload, mErr := json.Marshal(subscribePayload{Query: query, Variables: variables})
		if mErr != nil {
			b.mu.Unlock()
			return nil, nil, fmt.Errorf("graphql ingest broker: marshal subscribe: %w", mErr)
		}
		if err := b.writeJSONLocked(ctx, wsMessage{ID: subID, Type: msgSubscribe, Payload: subPayload}); err != nil {
			b.mu.Unlock()
			return nil, nil, fmt.Errorf("graphql ingest broker: send subscribe: %w", err)
		}
		f = &graphQLSubFanout{
			subID:   subID,
			key:     key,
			broker:  b,
			targets: map[uint64]chan *upstreamSubFrame{},
		}
		b.fanouts[key] = f
		b.recordOpenLocked()
	}
	b.mu.Unlock()

	// Attach a target to the fanout.
	f.mu.Lock()
	if f.terminal {
		// Upstream already terminated this fanout between the broker
		// lock release and now. Hand the caller a closed channel so
		// they observe immediate EOF. (Rare race; matches broker.go's
		// "release after terminal" handling.)
		f.mu.Unlock()
		ch := make(chan *upstreamSubFrame)
		close(ch)
		return ch, func() {}, nil
	}
	id := f.nextID
	f.nextID++
	ch := make(chan *upstreamSubFrame, 8)
	f.targets[id] = ch
	f.mu.Unlock()

	released := false
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		if released {
			releaseMu.Unlock()
			return
		}
		released = true
		releaseMu.Unlock()
		b.releaseTarget(f, id)
	}
	return ch, release, nil
}

// releaseTarget drops the (fanout, id) target. If the fanout has no
// remaining targets, sends `complete` upstream and removes the
// fanout. If the broker has no remaining fanouts, closes the WS.
func (b *graphQLSubBroker) releaseTarget(f *graphQLSubFanout, id uint64) {
	f.mu.Lock()
	if c, ok := f.targets[id]; ok {
		close(c)
		delete(f.targets, id)
	}
	empty := len(f.targets) == 0 && !f.terminal
	f.mu.Unlock()
	if !empty {
		return
	}

	// Last local consumer for this op — terminate upstream.
	b.mu.Lock()
	if cur, ok := b.fanouts[f.key]; ok && cur == f {
		delete(b.fanouts, f.key)
		_ = b.writeJSONLocked(context.Background(), wsMessage{ID: f.subID, Type: msgComplete})
		b.recordClosesLocked(1)
	}
	last := len(b.fanouts) == 0 && b.conn != nil
	b.mu.Unlock()
	if last {
		b.shutdown()
	}
}

// shutdown closes the upstream WS and resets broker state so the next
// acquire re-dials. Safe to call from any goroutine; idempotent.
func (b *graphQLSubBroker) shutdown() {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	b.closing = true
	conn := b.conn
	fanouts := b.fanouts
	b.fanouts = map[string]*graphQLSubFanout{}
	b.conn = nil
	b.recordClosesLocked(len(fanouts))
	b.mu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "broker shutdown")
	}
	for _, f := range fanouts {
		f.terminate(nil)
	}

	b.mu.Lock()
	b.closing = false
	b.mu.Unlock()
}

// dialAndInitLocked dials the upstream and runs the connection_init →
// connection_ack handshake. Caller holds b.mu.
func (b *graphQLSubBroker) dialAndInitLocked(
	ctx context.Context,
	endpoint string,
	forwardHeaders []string,
) (*websocket.Conn, error) {
	wsURL, err := graphqlEndpointToWSURL(endpoint)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	if in := HTTPRequestFromContext(ctx); in != nil {
		allow := forwardHeaders
		if allow == nil {
			allow = defaultForwardedHeaders
		}
		for _, h := range allow {
			if v := in.Header.Get(h); v != "" {
				headers.Set(h, v)
			}
		}
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{wsSubprotocol},
		HTTPHeader:   headers,
	})
	dialCancel()
	if err != nil {
		return nil, fmt.Errorf("graphql ws dial %s: %w", wsURL, err)
	}
	if conn.Subprotocol() != wsSubprotocol {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol mismatch")
		return nil, fmt.Errorf("graphql ws %s: server selected %q", wsURL, conn.Subprotocol())
	}

	// connection_init → connection_ack.
	if err := writeWSJSON(ctx, conn, &b.writeMu, wsMessage{Type: msgConnInit}); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "init failed")
		return nil, fmt.Errorf("graphql ws %s: write connection_init: %w", wsURL, err)
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, 5*time.Second)
	defer ackCancel()
	for {
		var m wsMessage
		if err := wsReadJSON(ackCtx, conn, &m); err != nil {
			_ = conn.Close(websocket.StatusInternalError, "ack timeout")
			return nil, fmt.Errorf("graphql ws %s: await connection_ack: %w", wsURL, err)
		}
		if m.Type == msgConnAck {
			return conn, nil
		}
		if m.Type == msgPing {
			_ = writeWSJSON(ctx, conn, &b.writeMu, wsMessage{Type: msgPong})
			continue
		}
		_ = conn.Close(websocket.StatusPolicyViolation, "pre-ack frame")
		return nil, fmt.Errorf("graphql ws %s: pre-ack frame %q", wsURL, m.Type)
	}
}

// writeJSONLocked sends a frame on the broker's conn. Caller holds
// b.mu (so b.conn won't race with shutdown). writeMu serializes the
// actual write.
func (b *graphQLSubBroker) writeJSONLocked(ctx context.Context, m wsMessage) error {
	if b.conn == nil {
		return fmt.Errorf("conn closed")
	}
	return writeWSJSON(ctx, b.conn, &b.writeMu, m)
}

// readerLoop pumps frames off the upstream WS and dispatches by
// subID into the matching fanout's targets.
func (b *graphQLSubBroker) readerLoop(conn *websocket.Conn) {
	for {
		var m wsMessage
		if err := wsReadJSON(context.Background(), conn, &m); err != nil {
			b.failAll(err)
			return
		}
		switch m.Type {
		case msgPing:
			_ = writeWSJSON(context.Background(), conn, &b.writeMu, wsMessage{Type: msgPong})
		case msgPong:
			// no-op
		case msgNext:
			b.dispatchNext(m.ID, m.Payload)
		case msgError:
			b.dispatchError(m.ID, m.Payload)
		case msgComplete:
			b.dispatchComplete(m.ID)
		}
	}
}

func (b *graphQLSubBroker) findFanoutBySubID(subID string) *graphQLSubFanout {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, f := range b.fanouts {
		if f.subID == subID {
			return f
		}
	}
	return nil
}

func (b *graphQLSubBroker) dispatchNext(subID string, payload []byte) {
	f := b.findFanoutBySubID(subID)
	if f == nil {
		return
	}
	var p struct {
		Data   map[string]any    `json:"data,omitempty"`
		Errors []json.RawMessage `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		f.fanout(&upstreamSubFrame{
			Errors: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"message":"decode next: %s"}`, jsonEscape(err.Error())))},
		})
		return
	}
	f.fanout(&upstreamSubFrame{Result: p.Data, Errors: p.Errors})
}

func (b *graphQLSubBroker) dispatchError(subID string, payload []byte) {
	f := b.findFanoutBySubID(subID)
	if f == nil {
		return
	}
	var errs []json.RawMessage
	if err := json.Unmarshal(payload, &errs); err != nil {
		errs = []json.RawMessage{payload}
	}
	f.terminate(&upstreamSubFrame{Errors: errs, Done: true})
	b.removeFanout(f)
}

func (b *graphQLSubBroker) dispatchComplete(subID string) {
	f := b.findFanoutBySubID(subID)
	if f == nil {
		return
	}
	f.terminate(&upstreamSubFrame{Done: true})
	b.removeFanout(f)
}

// removeFanout drops f from the broker's map. If the broker has no
// remaining fanouts, the WS is closed.
func (b *graphQLSubBroker) removeFanout(f *graphQLSubFanout) {
	b.mu.Lock()
	if cur, ok := b.fanouts[f.key]; ok && cur == f {
		delete(b.fanouts, f.key)
		b.recordClosesLocked(1)
	}
	last := len(b.fanouts) == 0 && b.conn != nil
	b.mu.Unlock()
	if last {
		b.shutdown()
	}
}

// failAll terminates every fanout with a synthetic transport error,
// closes the conn, and resets state.
func (b *graphQLSubBroker) failAll(err error) {
	frame := &upstreamSubFrame{
		Errors: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"message":"upstream ws read: %s"}`, jsonEscape(err.Error()))),
		},
		Done: true,
	}
	b.mu.Lock()
	fanouts := b.fanouts
	b.fanouts = map[string]*graphQLSubFanout{}
	conn := b.conn
	b.conn = nil
	b.recordClosesLocked(len(fanouts))
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusAbnormalClosure, "read error")
	}
	for _, f := range fanouts {
		f.terminate(frame)
	}
}

// fanout delivers a frame to every target (non-blocking).
func (f *graphQLSubFanout) fanout(frame *upstreamSubFrame) {
	f.mu.Lock()
	targets := make([]chan *upstreamSubFrame, 0, len(f.targets))
	for _, c := range f.targets {
		targets = append(targets, c)
	}
	f.mu.Unlock()
	for _, c := range targets {
		select {
		case c <- frame:
		default:
		}
	}
}

// terminate delivers a final frame (if non-nil) and closes every
// target. Subsequent acquires for the same key will create a fresh
// fanout. Safe to call multiple times — second call is a no-op.
func (f *graphQLSubFanout) terminate(final *upstreamSubFrame) {
	f.mu.Lock()
	if f.terminal {
		f.mu.Unlock()
		return
	}
	f.terminal = true
	targets := f.targets
	f.targets = map[uint64]chan *upstreamSubFrame{}
	f.mu.Unlock()
	for _, c := range targets {
		if final != nil {
			select {
			case c <- final:
			default:
			}
		}
		close(c)
	}
}

// fanoutKey is the dedup key for a (query, variables) pair. Same
// printed query + canonicalised variables → same key. Two callers
// with identical operations share a fanout. graphql-go's printer is
// deterministic; we order variables-map keys for stable JSON.
func fanoutKey(query string, variables map[string]any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(query))
	_, _ = h.Write([]byte{0})
	_ = canonicalJSON(h, variables)
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON writes a JSON encoding of v with map keys sorted at
// every level. Sufficient to make fanoutKey stable across equivalent
// variables maps; not a full JSON-canonical-form (numbers aren't
// re-formatted).
func canonicalJSON(w interface{ Write([]byte) (int, error) }, v any) error {
	switch x := v.(type) {
	case nil:
		_, _ = w.Write([]byte("null"))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_, _ = w.Write([]byte("{"))
		for i, k := range keys {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			kb, _ := json.Marshal(k)
			_, _ = w.Write(kb)
			_, _ = w.Write([]byte(":"))
			if err := canonicalJSON(w, x[k]); err != nil {
				return err
			}
		}
		_, _ = w.Write([]byte("}"))
	case []any:
		_, _ = w.Write([]byte("["))
		for i, e := range x {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			if err := canonicalJSON(w, e); err != nil {
				return err
			}
		}
		_, _ = w.Write([]byte("]"))
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		_, _ = w.Write(b)
	}
	return nil
}

// writeWSJSON is a serialised JSON write helper shared between the
// broker's dial-time handshake and runtime sends. The single-shot
// graphql_subscribe.go path has its own copy (wsWriteJSON); kept
// separate so that path stays usable without instantiating a broker.
func writeWSJSON(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex, m wsMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}
