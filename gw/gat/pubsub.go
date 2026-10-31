package gat

import (
	"strings"
	"sync"
	"sync/atomic"
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

// pubSub is gat's publish/subscribe layer: per-subscriber buffered
// queues (drop-oldest, subscriberQueueDepth) on top of a Bus. The Bus
// moves events (LocalBus by default; a peer mesh or NATS when
// configured); pubSub owns the queues and closes them on cancel or
// Gateway.Close.
//
// Concurrent-safe: Publish / Subscribe / a subscription's cancel may
// all be called from different goroutines.
type pubSub struct {
	bus atomic.Pointer[busBox]

	mu     sync.Mutex
	subs   map[int]*subscription
	nextID int
}

// busBox lets an interface value sit behind an atomic.Pointer, keeping
// the publish path lock-free.
type busBox struct{ Bus }

type subscription struct {
	ch    chan Event
	unsub func()
}

func newPubSub() *pubSub {
	ps := &pubSub{subs: map[int]*subscription{}}
	ps.bus.Store(&busBox{NewLocalBus()})
	return ps
}

// setBus swaps the transport. Subscriptions already open stay on the
// previous bus; call before serving traffic.
func (ps *pubSub) setBus(b Bus) {
	if b == nil {
		b = NewLocalBus()
	}
	ps.bus.Store(&busBox{b})
}

func (ps *pubSub) currentBus() Bus {
	return ps.bus.Load().Bus
}

// Subscribe registers a subscriber for every channel matching
// `pattern` (NATS-style: `*` matches one segment, `>` matches the
// rest). It returns a receive-only channel of events and a cancel
// func; calling cancel removes the subscription and closes the
// channel. The channel is buffered (subscriberQueueDepth); a consumer
// that falls behind loses the oldest events. If the bus rejects the
// pattern, the returned channel is already closed.
//
// Stability: experimental
func (ps *pubSub) Subscribe(pattern string) (<-chan Event, func()) {
	ch := make(chan Event, subscriberQueueDepth)
	unsub, err := ps.currentBus().Subscribe(pattern, func(channel string, payload []byte) {
		deliver(ch, Event{Channel: channel, Payload: payload})
	})
	if err != nil {
		close(ch)
		return ch, func() {}
	}
	ps.mu.Lock()
	id := ps.nextID
	ps.nextID++
	sub := &subscription{ch: ch, unsub: unsub}
	ps.subs[id] = sub
	ps.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			ps.mu.Lock()
			_, ok := ps.subs[id]
			delete(ps.subs, id)
			ps.mu.Unlock()
			if ok {
				sub.unsub()
				close(sub.ch)
			}
		})
	}
	return sub.ch, cancel
}

// closeAll cancels every active subscription and closes its channel.
// Called by Gateway.Close; after it runs, Publish reaches no existing
// subscriber and Subscribe still works on a fresh subscription.
func (ps *pubSub) closeAll() {
	ps.mu.Lock()
	subs := ps.subs
	ps.subs = map[int]*subscription{}
	ps.mu.Unlock()
	for _, sub := range subs {
		sub.unsub()
		close(sub.ch)
	}
}

// Publish hands the event to the bus. Delivery is best-effort; a bus
// error (e.g. a disconnected transport) drops the event.
//
// Stability: experimental
func (ps *pubSub) Publish(channel string, payload []byte) {
	_ = ps.currentBus().Publish(channel, payload)
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

// matchChannel is the tokenised form of MatchChannel, kept for the
// table test that predates it.
func matchChannel(pattern, channel []string) bool {
	return MatchChannel(strings.Join(pattern, "."), strings.Join(channel, "."))
}
