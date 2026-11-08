package events

import (
	"context"
	"fmt"
	"log"
	"sync"

	"google.golang.org/protobuf/proto"
)

// Core is what a generated Bus wraps: it encodes, fills subjects,
// records subscriptions and calls the Backend.
//
// Stability: experimental
type Core struct {
	backend Backend
	serde   Serde
	logf    func(format string, args ...any)

	mu   sync.Mutex
	subs []Subscription

	warned sync.Map // "channel\x00type" -> struct{}
}

// Option configures a Core (and so a generated Bus).
//
// Stability: experimental
type Option func(*Core)

// WithLogf sets the logger for skipped messages. Default log.Printf.
//
// Stability: experimental
func WithLogf(f func(format string, args ...any)) Option {
	return func(c *Core) { c.logf = f }
}

// NewCore returns a Core over b.
//
// Stability: experimental
func NewCore(b Backend, opts ...Option) *Core {
	c := &Core{backend: b, serde: ProtoSerde{}, logf: log.Printf}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Backend returns the backend.
func (c *Core) Backend() Backend { return c.backend }

// Serde returns the serde.
func (c *Core) Serde() Serde { return c.serde }

func (c *Core) topic(ch Channel) string {
	if t, ok := c.backend.(Topics); ok {
		return t.Topic(ch)
	}
	return ch.Stream
}

// Publish encodes m and publishes it on the subject built from
// segments (in pattern order).
func (c *Core) Publish(ctx context.Context, ch Channel, segments []string, m proto.Message, opts ...PublishOption) error {
	if m == nil {
		return fmt.Errorf("publish %s: nil message", ch.Key)
	}
	subject, err := ch.Pattern.Subject(segments)
	if err != nil {
		return fmt.Errorf("publish %s: %w", ch.Key, err)
	}
	var po publishOptions
	for _, o := range opts {
		o(&po)
	}
	if po.id == "" {
		po.id = NewID()
	}
	headers := make(map[string]string, len(po.headers)+2)
	for k, v := range po.headers {
		headers[k] = v
	}
	headers[HeaderType] = string(m.ProtoReflect().Descriptor().FullName())
	headers[HeaderChannel] = ch.Key
	msg := &Message{Subject: subject, ID: po.id, Headers: headers}
	if err := c.serde.Encode(ctx, ch, c.topic(ch), m, msg); err != nil {
		return fmt.Errorf("publish %s: encode: %w", ch.Key, err)
	}
	return c.backend.Publish(ctx, ch, msg)
}

// SegmentHandler gets the wildcard segment values (pattern order) of
// the delivered subject.
//
// Stability: experimental
type SegmentHandler func(ctx context.Context, segments []string, m *Message) error

// Subscribe listens on ch as group. Messages whose subject does not
// match the pattern are skipped (acked). The handler's ctx carries
// the message for MessageFrom.
func (c *Core) Subscribe(ctx context.Context, ch Channel, group string, h SegmentHandler, opts ...SubscribeOption) (stop func(), err error) {
	if !ValidGroup(group) {
		return nil, fmt.Errorf("subscribe %s: group %q: use letters, digits, '_' or '-'", ch.Key, group)
	}
	o := ResolveSubscribeOptions(opts...)
	topic := c.topic(ch)
	stop, err = c.backend.Subscribe(ctx, ch, group, o, func(ctx context.Context, raw *Message) error {
		m := raw
		if _, plain := c.serde.(ProtoSerde); !plain {
			m = raw.clone()
			if err := c.serde.Decode(ctx, ch, topic, m); err != nil {
				return fmt.Errorf("decode %s: %w", ch.Key, err)
			}
		}
		segs, ok := ch.Pattern.Match(m.Subject)
		if !ok {
			c.Skip(ch, m, "subject "+m.Subject+" does not match "+ch.Pattern.String())
			return nil
		}
		return h(WithMessage(ctx, m), segs, m)
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	sub := Subscription{Channel: ch.Key, Group: group}
	found := false
	for _, s := range c.subs {
		if s == sub {
			found = true
			break
		}
	}
	if !found {
		c.subs = append(c.subs, sub)
	}
	c.mu.Unlock()
	return stop, nil
}

// Skip logs, once per (channel, message type), that a message was
// acked without a handler.
func (c *Core) Skip(ch Channel, m *Message, why string) {
	k := ch.Key + "\x00" + m.Type()
	if _, loaded := c.warned.LoadOrStore(k, struct{}{}); loaded {
		return
	}
	c.logf("events: %s: skipped message type %q: %s (logged once)", ch.Key, m.Type(), why)
}

// Subscriptions returns every (channel, group) Subscribe was called
// with, in call order. Stopping a subscriber does not remove it.
func (c *Core) Subscriptions() []Subscription {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Subscription(nil), c.subs...)
}
