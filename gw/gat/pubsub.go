package gat

import (
	"strings"
	"sync"
)

// Event is one published message. Channel is the concrete dotted
// channel name it was published on; Payload is the opaque body.
// The JSON tags are the wire shape used by the SSE subscribe stream
// and the peer-mesh transport.
//
// Stability: experimental
type Event struct {
	Channel string `json:"channel"`
	Payload []byte `json:"payload"`
}

// subscriberQueueDepth bounds each subscriber's buffered channel.
// Delivery is best-effort: a subscriber that doesn't keep up has the
// oldest queued event dropped rather than stalling the publisher.
const subscriberQueueDepth = 64

// pubSub is gat's in-process publish/subscribe primitive: a registry
// of pattern subscribers with NATS-style channel matching. It is the
// single-node foundation — peer fanout (cross-node) layers on top by
// calling publishLocal on received events.
//
// Concurrent-safe: Publish / Subscribe / a subscription's cancel may
// all be called from different goroutines.
type pubSub struct {
	mu     sync.RWMutex
	subs   map[int]*subscription
	nextID int
}

type subscription struct {
	id      int
	pattern []string // tokenised match pattern
	ch      chan Event
}

func newPubSub() *pubSub {
	return &pubSub{subs: map[int]*subscription{}}
}

// Subscribe registers a subscriber for every channel matching
// `pattern` (NATS-style: `*` matches one segment, `>` matches the
// rest). It returns a receive-only channel of events and a cancel
// func; calling cancel removes the subscription and closes the
// channel. The channel is buffered (subscriberQueueDepth); a consumer
// that falls behind loses the oldest events.
//
// Stability: experimental
func (ps *pubSub) Subscribe(pattern string) (<-chan Event, func()) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	id := ps.nextID
	ps.nextID++
	sub := &subscription{
		id:      id,
		pattern: strings.Split(pattern, "."),
		ch:      make(chan Event, subscriberQueueDepth),
	}
	ps.subs[id] = sub
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			ps.mu.Lock()
			defer ps.mu.Unlock()
			if _, ok := ps.subs[id]; ok {
				delete(ps.subs, id)
				close(sub.ch)
			}
		})
	}
	return sub.ch, cancel
}

// closeAll cancels every active subscription and closes its channel.
// Called by Gateway.Close; after it runs, Publish is a no-op (no
// subscribers) and Subscribe still works on a fresh subscription.
func (ps *pubSub) closeAll() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for id, sub := range ps.subs {
		delete(ps.subs, id)
		close(sub.ch)
	}
}

// Publish delivers payload to every local subscriber whose pattern
// matches `channel`. This is the public entry point — for a gateway
// with peers, Publish also triggers cross-node fanout (see
// peerPublish). publishLocal is the local-only half, reused by the
// peer receive handler so a received event fans out without
// re-broadcasting.
//
// Stability: experimental
func (ps *pubSub) Publish(channel string, payload []byte) {
	ps.publishLocal(channel, payload)
}

// publishLocal fans an event out to matching local subscribers only.
// Send is non-blocking: if a subscriber's buffer is full the oldest
// event is dropped to make room, keeping the publisher unblocked.
func (ps *pubSub) publishLocal(channel string, payload []byte) {
	segments := strings.Split(channel, ".")
	ps.mu.RLock()
	targets := make([]*subscription, 0, len(ps.subs))
	for _, sub := range ps.subs {
		if matchChannel(sub.pattern, segments) {
			targets = append(targets, sub)
		}
	}
	ps.mu.RUnlock()

	ev := Event{Channel: channel, Payload: payload}
	for _, sub := range targets {
		deliver(sub.ch, ev)
	}
}

// deliver does a best-effort send: if ch is full, drop the oldest
// buffered event and retry once. A subscription cancelled between
// snapshot and send has a closed ch — the recover guards that race.
func deliver(ch chan Event, ev Event) {
	defer func() { _ = recover() }() // ch closed by a concurrent cancel
	select {
	case ch <- ev:
	default:
		select {
		case <-ch: // drop oldest
		default:
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

// matchChannel reports whether a tokenised subscription pattern
// matches a tokenised concrete channel. `*` matches exactly one
// segment; `>` matches one or more trailing segments and must be the
// final pattern token.
func matchChannel(pattern, channel []string) bool {
	for i, tok := range pattern {
		if tok == ">" {
			// `>` must be last and needs at least one segment to cover.
			return i == len(pattern)-1 && i < len(channel)
		}
		if i >= len(channel) {
			return false
		}
		if tok != "*" && tok != channel[i] {
			return false
		}
	}
	return len(pattern) == len(channel)
}
