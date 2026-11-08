package gat

import (
	"errors"
	"sync"
)

// Bus is the transport under gat's (and gwag's) publish/subscribe
// layer. It moves opaque payloads between publishers and pattern
// subscribers; it does not buffer, authenticate, or decode. Those
// concerns stay above the Bus (gat: subscribe-token check and the
// per-subscriber drop-oldest queue; gwag: HMAC channel auth, channel
// bindings, refcounted decode-once fanout).
//
// Channels are dot-separated (`orders.42.created`). Patterns use NATS
// wildcard rules: `*` matches exactly one segment, `>` matches one or
// more trailing segments and must be the last token. MatchChannel is
// the reference matcher.
//
// Publish delivers payload to every subscription whose pattern
// matches channel. It may return before delivery completes; delivery
// is best-effort and an implementation may drop under load or while
// disconnected.
//
// Subscribe calls fn once per matching publish. fn may run on the
// publisher's goroutine (LocalBus) or on a transport goroutine (NATS),
// so it must not block: hand the event to a buffered queue and
// return. fn may still be called briefly after unsubscribe returns,
// while an in-flight delivery finishes; callers must tolerate that.
// unsubscribe is idempotent. payload must not be retained or modified
// by fn beyond the call unless the implementation documents
// otherwise.
//
// Implementations: NewLocalBus (in-process), NewPeerMeshBus (HTTP
// fanout to peer gat instances), and gatnats.New (NATS) in the
// gw/gat/gatnats subpackage.
//
// Stability: experimental
type Bus interface {
	Publish(channel string, payload []byte) error
	Subscribe(pattern string, fn func(channel string, payload []byte)) (unsubscribe func(), err error)
}

// Header is a set of message headers: NATS headers on gatnats, carried
// in memory on LocalBus. Keys are used as given (no canonicalisation),
// so "Nats-Msg-Id" stays "Nats-Msg-Id". The underlying type matches
// nats.Header and http.Header, so a conversion is free.
//
// Stability: experimental
type Header map[string][]string

// Get returns the first value for key, or "".
//
// Stability: experimental
func (h Header) Get(key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Set replaces the values for key with value.
//
// Stability: experimental
func (h Header) Set(key, value string) { h[key] = []string{value} }

// HeaderBus is an optional Bus extension for transports that carry
// headers with each payload. Callers type-assert a Bus to HeaderBus.
// A plain Subscribe on a HeaderBus still sees every publish, headers
// dropped; a plain Publish arrives at SubscribeMsg with a nil Header.
//
// LocalBus and gatnats.Bus implement it; NewPeerMeshBus does not.
//
// Stability: experimental
type HeaderBus interface {
	Bus
	PublishMsg(channel string, header Header, payload []byte) error
	SubscribeMsg(pattern string, fn func(channel string, header Header, payload []byte)) (unsubscribe func(), err error)
}

// ErrEmptyChannel is returned by LocalBus when Publish gets an empty
// channel or Subscribe gets an empty pattern.
//
// Stability: experimental
var ErrEmptyChannel = errors.New("gat: empty channel")

// LocalBus is the in-process Bus: a registry of pattern subscribers
// in one process. Publish calls each matching fn synchronously on the
// publisher's goroutine. It is gat's default bus and gwag's bus when
// no cluster is configured or NATS is down.
//
// Concurrent-safe.
//
// Stability: experimental
type LocalBus struct {
	mu     sync.RWMutex
	subs   map[uint64]*localSub
	nextID uint64
}

type localSub struct {
	pattern string
	fn      func(channel string, header Header, payload []byte)
}

var _ HeaderBus = (*LocalBus)(nil)

// NewLocalBus returns an empty in-process bus.
//
// Stability: experimental
func NewLocalBus() *LocalBus {
	return &LocalBus{subs: map[uint64]*localSub{}}
}

// Publish calls fn for every subscription whose pattern matches
// channel. The subscriber set is snapshotted first, so fn may call
// Subscribe or unsubscribe without deadlocking.
//
// Stability: experimental
func (b *LocalBus) Publish(channel string, payload []byte) error {
	return b.PublishMsg(channel, nil, payload)
}

// PublishMsg is Publish with headers. Every subscriber gets the same
// Header map; fn must not modify it.
//
// Stability: experimental
func (b *LocalBus) PublishMsg(channel string, header Header, payload []byte) error {
	if channel == "" {
		return ErrEmptyChannel
	}
	b.mu.RLock()
	if len(b.subs) == 0 {
		b.mu.RUnlock()
		return nil
	}
	targets := make([]func(string, Header, []byte), 0, len(b.subs))
	for _, s := range b.subs {
		if MatchChannel(s.pattern, channel) {
			targets = append(targets, s.fn)
		}
	}
	b.mu.RUnlock()
	for _, fn := range targets {
		fn(channel, header, payload)
	}
	return nil
}

// Subscribe registers fn for every channel matching pattern.
//
// Stability: experimental
func (b *LocalBus) Subscribe(pattern string, fn func(channel string, payload []byte)) (func(), error) {
	return b.SubscribeMsg(pattern, func(channel string, _ Header, payload []byte) {
		fn(channel, payload)
	})
}

// SubscribeMsg is Subscribe with headers.
//
// Stability: experimental
func (b *LocalBus) SubscribeMsg(pattern string, fn func(channel string, header Header, payload []byte)) (func(), error) {
	if pattern == "" {
		return nil, ErrEmptyChannel
	}
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = &localSub{pattern: pattern, fn: fn}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
		})
	}, nil
}

// MatchChannel reports whether the NATS-style pattern matches the
// concrete channel. `*` matches exactly one segment; `>` matches one
// or more trailing segments and only as the last pattern token (a `>`
// anywhere else matches nothing, as NATS rejects such a subject). An
// empty pattern or channel never matches.
//
//	MatchChannel("orders.>", "orders.42.created") == true
//	MatchChannel("orders.*", "orders.42")         == true
//	MatchChannel("orders.>", "orders")            == false
//
// Allocation-free.
//
// Stability: experimental
func MatchChannel(pattern, channel string) bool {
	if pattern == "" || channel == "" {
		return false
	}
	// pi / ci index the start of the next pattern / channel segment;
	// ci > len(channel) means the channel is used up.
	pi, ci := 0, 0
	for {
		pe := pi
		for pe < len(pattern) && pattern[pe] != '.' {
			pe++
		}
		seg := pattern[pi:pe]
		pLast := pe == len(pattern)
		if seg == ">" {
			return pLast && ci <= len(channel)
		}
		if ci > len(channel) {
			return false
		}
		ce := ci
		for ce < len(channel) && channel[ce] != '.' {
			ce++
		}
		if seg != "*" && seg != channel[ci:ce] {
			return false
		}
		ci = ce + 1
		if pLast {
			return ci > len(channel)
		}
		pi = pe + 1
	}
}
