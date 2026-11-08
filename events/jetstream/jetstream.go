// Package jetstream is the NATS JetStream events.Backend.
//
// One stream per channel, named events.StreamName(key), capturing the
// channel's wildcard subject. The backend creates a missing stream
// (file storage, limits retention, MaxAge 7d, Duplicates 2m, 1
// replica) and never updates an existing one: the gateway owns the
// stream's cluster policy. A group is a durable consumer on that
// stream. The message id rides in Nats-Msg-Id, so republishing an id
// inside the 2m window stores it once.
//
// Stability: experimental
package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/iodesystems/gwag/v2/events"
)

// Backend is the JetStream backend.
//
// Stability: experimental
type Backend struct {
	nc       *nats.Conn
	js       jetstream.JetStream
	logf     func(format string, args ...any)
	prefetch int

	ensured sync.Map // stream name -> struct{}

	mu     sync.Mutex
	closed bool
	stops  map[int]context.CancelFunc
	nextID int
}

// Option configures a Backend.
//
// Stability: experimental
type Option func(*Backend)

// WithLogf sets the logger for dead-letter publish failures. Default
// log.Printf.
//
// Stability: experimental
func WithLogf(f func(format string, args ...any)) Option { return func(b *Backend) { b.logf = f } }

// WithPrefetch sets how many messages a subscriber pulls ahead.
// Default 16. Lower spreads a group's work more evenly across
// subscribers; prefetched messages count against AckWait.
//
// Stability: experimental
func WithPrefetch(n int) Option { return func(b *Backend) { b.prefetch = n } }

// New returns a backend over nc. Close does not close nc.
//
// Stability: experimental
func New(nc *nats.Conn, opts ...Option) (*Backend, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("events/jetstream: %w", err)
	}
	b := &Backend{nc: nc, js: js, logf: log.Printf, prefetch: 16, stops: map[int]context.CancelFunc{}}
	for _, o := range opts {
		o(b)
	}
	if b.prefetch < 1 {
		b.prefetch = 1
	}
	return b, nil
}

// StreamConfig is the config the backend creates a missing stream
// with.
//
// Stability: experimental
func StreamConfig(ch events.Channel) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:       ch.Stream,
		Subjects:   []string{ch.Pattern.Wildcard()},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     7 * 24 * time.Hour,
		Duplicates: 2 * time.Minute,
		Replicas:   1,
	}
}

func (b *Backend) ensureStream(ctx context.Context, ch events.Channel) error {
	if _, ok := b.ensured.Load(ch.Stream); ok {
		return nil
	}
	_, err := b.js.Stream(ctx, ch.Stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		_, err = b.js.CreateStream(ctx, StreamConfig(ch))
		if errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			err = nil
		}
	}
	if err != nil {
		return fmt.Errorf("events/jetstream: stream %s: %w", ch.Stream, err)
	}
	b.ensured.Store(ch.Stream, struct{}{})
	return nil
}

// Publish implements events.Backend.
func (b *Backend) Publish(ctx context.Context, ch events.Channel, m *events.Message) error {
	if err := b.ensureStream(ctx, ch); err != nil {
		return err
	}
	msg := nats.NewMsg(m.Subject)
	for k, v := range m.Headers {
		msg.Header.Set(k, v)
	}
	if m.ID != "" {
		msg.Header.Set(events.HeaderNatsMsgID, m.ID)
	}
	msg.Data = m.Data
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("events/jetstream: publish %s: %w", m.Subject, err)
	}
	return nil
}

// Subscribe implements events.Backend. The group is a durable
// consumer; an existing one is used as is.
func (b *Backend) Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (func(), error) {
	if err := b.ensureStream(ctx, ch); err != nil {
		return nil, err
	}
	cons, err := b.consumer(ctx, ch, group, opts)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		b.handle(ctx, ch, opts, h, msg)
	}, jetstream.PullMaxMessages(b.prefetch))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("events/jetstream: consume %s/%s: %w", ch.Stream, group, err)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		cc.Stop()
		return nil, errors.New("events/jetstream: closed")
	}
	id := b.nextID
	b.nextID++
	b.stops[id] = cancel
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		cc.Stop()
		b.mu.Lock()
		delete(b.stops, id)
		b.mu.Unlock()
	}()
	return cancel, nil
}

func (b *Backend) consumer(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions) (jetstream.Consumer, error) {
	cons, err := b.js.Consumer(ctx, ch.Stream, group)
	if err == nil {
		return cons, nil
	}
	if !errors.Is(err, jetstream.ErrConsumerNotFound) {
		return nil, fmt.Errorf("events/jetstream: consumer %s/%s: %w", ch.Stream, group, err)
	}
	deliver := jetstream.DeliverNewPolicy
	if opts.DeliverAll {
		deliver = jetstream.DeliverAllPolicy
	}
	cons, err = b.js.CreateConsumer(ctx, ch.Stream, jetstream.ConsumerConfig{
		Durable:       group,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: deliver,
		MaxDeliver:    opts.MaxDeliver,
		AckWait:       opts.AckWait,
	})
	if errors.Is(err, jetstream.ErrConsumerExists) {
		cons, err = b.js.Consumer(ctx, ch.Stream, group)
	}
	if err != nil {
		return nil, fmt.Errorf("events/jetstream: create consumer %s/%s: %w", ch.Stream, group, err)
	}
	return cons, nil
}

func (b *Backend) handle(ctx context.Context, ch events.Channel, opts events.SubscribeOptions, h events.Handler, msg jetstream.Msg) {
	m := &events.Message{
		Subject: msg.Subject(),
		Headers: map[string]string{},
		Data:    msg.Data(),
	}
	for k, v := range msg.Headers() {
		if len(v) > 0 {
			m.Headers[k] = v[0]
		}
	}
	m.ID = m.Headers[events.HeaderNatsMsgID]
	delete(m.Headers, events.HeaderNatsMsgID)
	if md, err := msg.Metadata(); err == nil {
		m.Delivery = int(md.NumDelivered)
	}
	err := h(ctx, m)
	if err == nil {
		_ = msg.Ack()
		return
	}
	if m.Delivery >= opts.MaxDeliver {
		b.deadLetter(ctx, ch, msg, err)
		_ = msg.Term()
		return
	}
	_ = msg.NakWithDelay(opts.Delay(m.Delivery))
}

func (b *Backend) deadLetter(ctx context.Context, ch events.Channel, msg jetstream.Msg, cause error) {
	dead := nats.NewMsg(events.DLQSubject(ch.Stream))
	for k, v := range msg.Headers() {
		dead.Header[k] = append([]string(nil), v...)
	}
	dead.Header.Set(events.HeaderError, cause.Error())
	dead.Data = msg.Data()
	// A stream may capture gwag.dlq.>; without one, fall back to a
	// plain publish so live listeners still see it.
	_, err := b.js.PublishMsg(ctx, dead)
	if errors.Is(err, jetstream.ErrNoStreamResponse) || errors.Is(err, nats.ErrNoResponders) {
		err = b.nc.PublishMsg(dead)
	}
	if err != nil {
		b.logf("events/jetstream: dead-letter %s: %v", msg.Subject(), err)
	}
}

// Close stops every subscriber started by this backend.
func (b *Backend) Close() error {
	b.mu.Lock()
	b.closed = true
	stops := make([]context.CancelFunc, 0, len(b.stops))
	for _, s := range b.stops {
		stops = append(stops, s)
	}
	b.mu.Unlock()
	for _, s := range stops {
		s()
	}
	return nil
}
