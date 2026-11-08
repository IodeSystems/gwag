// Package memory is an in-process events.Backend for tests.
//
// Semantics follow the JetStream backend: each group gets every
// message published after the group's first subscriber (or every
// stored message with DeliverAll); subscribers sharing a group share
// its queue; failed deliveries come back after the backoff; after
// MaxDeliver failures the message goes to DeadLetters. Message ids
// dedup within a 2 minute window. AckWait is not modelled: a handler
// runs until it returns.
//
// Stability: experimental
package memory

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iodesystems/gwag/v2/events"
)

// DedupWindow is how long a message id is remembered.
const DedupWindow = 2 * time.Minute

// Backend is the in-process backend. The zero value is not usable;
// call New.
//
// Stability: experimental
type Backend struct {
	mu       sync.Mutex
	closed   bool
	done     chan struct{}
	channels map[string]*channel // by stream
	seen     map[string]time.Time
	dlq      []*events.Message
	now      func() time.Time
}

type channel struct {
	log    []*events.Message
	groups map[string]*group
}

type group struct {
	queue  []item
	signal chan struct{}
}

type item struct {
	m        *events.Message
	delivery int
}

// New returns an empty backend.
//
// Stability: experimental
func New() *Backend {
	return &Backend{done: make(chan struct{}), channels: map[string]*channel{}, seen: map[string]time.Time{}, now: time.Now}
}

var errClosed = errors.New("memory backend: closed")

func (b *Backend) channelLocked(ch events.Channel) *channel {
	c := b.channels[ch.Stream]
	if c == nil {
		c = &channel{groups: map[string]*group{}}
		b.channels[ch.Stream] = c
	}
	return c
}

// Publish implements events.Backend.
func (b *Backend) Publish(_ context.Context, ch events.Channel, m *events.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errClosed
	}
	now := b.now()
	if m.ID != "" {
		k := ch.Stream + "\x00" + m.ID
		if t, ok := b.seen[k]; ok && now.Sub(t) < DedupWindow {
			return nil
		}
		b.seen[k] = now
	}
	stored := clone(m)
	c := b.channelLocked(ch)
	c.log = append(c.log, stored)
	for _, g := range c.groups {
		g.push(item{m: stored, delivery: 1})
	}
	return nil
}

func (g *group) push(it item) {
	g.queue = append(g.queue, it)
	select {
	case g.signal <- struct{}{}:
	default:
	}
}

// Subscribe implements events.Backend.
func (b *Backend) Subscribe(ctx context.Context, ch events.Channel, groupName string, opts events.SubscribeOptions, h events.Handler) (func(), error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errClosed
	}
	c := b.channelLocked(ch)
	g := c.groups[groupName]
	if g == nil {
		g = &group{signal: make(chan struct{}, 1)}
		c.groups[groupName] = g
		if opts.DeliverAll {
			for _, m := range c.log {
				g.push(item{m: m, delivery: 1})
			}
		}
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	go b.work(ctx, ch, g, opts, h)
	return cancel, nil
}

func (b *Backend) work(ctx context.Context, ch events.Channel, g *group, opts events.SubscribeOptions, h events.Handler) {
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		var it item
		have := len(g.queue) > 0
		if have {
			it = g.queue[0]
			g.queue = g.queue[1:]
		}
		b.mu.Unlock()
		if !have {
			select {
			case <-ctx.Done():
				return
			case <-b.done:
				return
			case <-g.signal:
			}
			continue
		}
		if ctx.Err() != nil {
			// Stopped while holding an item: put it back for the group.
			b.mu.Lock()
			g.queue = append([]item{it}, g.queue...)
			b.mu.Unlock()
			return
		}
		m := clone(it.m)
		m.Delivery = it.delivery
		err := h(ctx, m)
		if err == nil {
			continue
		}
		if it.delivery >= opts.MaxDeliver {
			dead := clone(it.m)
			dead.Subject = events.DLQSubject(ch.Stream)
			dead.Headers[events.HeaderError] = err.Error()
			dead.Delivery = it.delivery
			b.mu.Lock()
			b.dlq = append(b.dlq, dead)
			b.mu.Unlock()
			continue
		}
		next := item{m: it.m, delivery: it.delivery + 1}
		time.AfterFunc(opts.Delay(it.delivery), func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if !b.closed {
				g.push(next)
			}
		})
	}
}

// DeadLetters returns the dead-lettered messages, oldest first. Each
// has Subject = events.DLQSubject(stream) and the HeaderError header.
func (b *Backend) DeadLetters() []*events.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*events.Message, len(b.dlq))
	for i, m := range b.dlq {
		out[i] = clone(m)
	}
	return out
}

// Close stops every subscriber.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.done)
	return nil
}

func clone(m *events.Message) *events.Message {
	out := *m
	out.Headers = make(map[string]string, len(m.Headers))
	for k, v := range m.Headers {
		out.Headers[k] = v
	}
	out.Data = append([]byte(nil), m.Data...)
	return &out
}
