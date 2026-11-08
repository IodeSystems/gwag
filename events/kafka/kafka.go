// Package kafka is an events.Backend over Apache Kafka (franz-go). It
// is its own Go module (github.com/iodesystems/gwag/v2/events/kafka),
// so the gwag root module carries no Kafka dependency.
//
// Mapping:
//   - one topic per channel, named ch.Stream (EV_<pkg>_<Name>),
//     created with Partitions partitions if missing;
//   - record key = subject (ordering per subject), value = proto body,
//     headers = Gwag-Type, Gwag-Channel, Gwag-Msg-Id and extras;
//   - group = Kafka consumer group "<stream>.<group>";
//   - a failed handler is retried in process with the backoff (the
//     partition waits), then the record goes to topic
//     gwag.dlq.<stream> with Gwag-Error and the offset is committed.
//
// No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
// seen. Handlers must be idempotent.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/iodesystems/gwag/v2/events"
)

// Backend is the Kafka backend.
type Backend struct {
	brokers    []string
	partitions int32
	producer   *kgo.Client
	admin      *kadm.Client

	topics sync.Map // topic -> struct{}

	mu     sync.Mutex
	subs   map[*kgo.Client]context.CancelFunc
	closed bool
}

// Option configures a Backend.
type Option func(*Backend)

// WithPartitions sets the partition count for topics the backend
// creates. Default 3. A group spreads over at most this many
// subscribers.
func WithPartitions(n int32) Option { return func(b *Backend) { b.partitions = n } }

// New connects to brokers.
func New(brokers []string, opts ...Option) (*Backend, error) {
	b := &Backend{brokers: brokers, partitions: 3, subs: map[*kgo.Client]context.CancelFunc{}}
	for _, o := range opts {
		o(b)
	}
	p, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.AllowAutoTopicCreation())
	if err != nil {
		return nil, err
	}
	b.producer = p
	b.admin = kadm.NewClient(p)
	return b, nil
}

func (b *Backend) ensureTopic(ctx context.Context, topic string) error {
	if _, ok := b.topics.Load(topic); ok {
		return nil
	}
	resp, err := b.admin.CreateTopic(ctx, b.partitions, 1, nil, topic)
	if err == nil {
		err = resp.Err
	}
	if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
		return fmt.Errorf("kafka: create topic %s: %w", topic, err)
	}
	b.topics.Store(topic, struct{}{})
	return nil
}

// seedGroup commits the start position of a group that has none yet
// (the end of each partition, or the start with deliverAll). Without
// it a new group resolves its position after the partition assignment,
// so a record produced between assignment and that lookup is skipped
// even though Subscribe has returned. A group that already holds
// commits keeps them. Where the commit is refused because the group
// is live, its members hold their positions already.
func (b *Backend) seedGroup(ctx context.Context, topic, group string, deliverAll bool) error {
	var committed kadm.OffsetResponses
	err := retryMetadata(ctx, func() (err error) {
		committed, err = b.admin.FetchOffsets(ctx, group)
		return err
	})
	if err != nil {
		return fmt.Errorf("kafka: fetch offsets of %s: %w", group, err)
	}
	var listed kadm.ListedOffsets
	err = retryMetadata(ctx, func() error {
		var err error
		if deliverAll {
			listed, err = b.admin.ListStartOffsets(ctx, topic)
		} else {
			listed, err = b.admin.ListEndOffsets(ctx, topic)
		}
		if err == nil {
			err = listed.Error()
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("kafka: list offsets of %s: %w", topic, err)
	}
	seed := listed.Offsets()
	for p := range seed[topic] {
		if o, ok := committed.Lookup(topic, p); ok && o.At >= 0 {
			seed.Delete(topic, p)
		}
	}
	if len(seed[topic]) == 0 {
		return nil
	}
	_, _ = b.admin.CommitOffsets(ctx, group, seed)
	return nil
}

// retryMetadata runs fn until it succeeds, fails with an error that
// is not about the broker still starting up, or 30s pass. A new topic
// has no leader everywhere yet, and a fresh broker has no group
// coordinator until __consumer_offsets is created.
func retryMetadata(ctx context.Context, fn func() error) error {
	for deadline := time.Now().Add(30 * time.Second); ; {
		err := fn()
		if err == nil {
			return nil
		}
		starting := errors.Is(err, kerr.UnknownTopicOrPartition) || errors.Is(err, kerr.LeaderNotAvailable) ||
			errors.Is(err, kerr.NotLeaderForPartition) || errors.Is(err, kerr.CoordinatorNotAvailable) ||
			errors.Is(err, kerr.CoordinatorLoadInProgress) || errors.Is(err, kerr.NotCoordinator)
		if !starting || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func record(topic string, m *events.Message) *kgo.Record {
	r := &kgo.Record{Topic: topic, Key: []byte(m.Subject), Value: m.Data}
	for k, v := range m.Headers {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
	}
	if m.ID != "" {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: events.HeaderMsgID, Value: []byte(m.ID)})
	}
	return r
}

// Publish implements events.Backend.
func (b *Backend) Publish(ctx context.Context, ch events.Channel, m *events.Message) error {
	if err := b.ensureTopic(ctx, ch.Stream); err != nil {
		return err
	}
	return b.producer.ProduceSync(ctx, record(ch.Stream, m)).FirstErr()
}

// Subscribe implements events.Backend. It returns once the consumer
// holds a partition assignment.
func (b *Backend) Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (func(), error) {
	if err := b.ensureTopic(ctx, ch.Stream); err != nil {
		return nil, err
	}
	// Create the DLQ topic now. A reader of gwag.dlq.<stream> that
	// starts before the first dead letter would otherwise wait for a
	// metadata refresh to find a topic created after it.
	if err := b.ensureTopic(ctx, events.DLQSubject(ch.Stream)); err != nil {
		return nil, err
	}
	if err := b.seedGroup(ctx, ch.Stream, ch.Stream+"."+group, opts.DeliverAll); err != nil {
		return nil, err
	}
	reset := kgo.NewOffset().AtEnd()
	if opts.DeliverAll {
		reset = kgo.NewOffset().AtStart()
	}
	assigned := make(chan struct{})
	var once sync.Once
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(b.brokers...),
		kgo.ConsumerGroup(ch.Stream+"."+group),
		kgo.ConsumeTopics(ch.Stream),
		kgo.ConsumeResetOffset(reset),
		kgo.DisableAutoCommit(),
		kgo.OnPartitionsAssigned(func(context.Context, *kgo.Client, map[string][]int32) {
			once.Do(func() { close(assigned) })
		}),
	)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.subs[cl] = cancel
	b.mu.Unlock()
	go b.consume(ctx, cl, ch, opts, h)
	select {
	case <-assigned:
	case <-time.After(30 * time.Second):
		cancel()
		return nil, fmt.Errorf("kafka: group %s.%s: no partition assignment after 30s", ch.Stream, group)
	}
	return cancel, nil
}

func (b *Backend) consume(ctx context.Context, cl *kgo.Client, ch events.Channel, opts events.SubscribeOptions, h events.Handler) {
	defer func() {
		cl.Close()
		b.mu.Lock()
		delete(b.subs, cl)
		b.mu.Unlock()
	}()
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachRecord(func(r *kgo.Record) { b.handle(ctx, ch, opts, h, r) })
		if ctx.Err() != nil {
			return
		}
		_ = cl.CommitUncommittedOffsets(ctx)
	}
}

func (b *Backend) handle(ctx context.Context, ch events.Channel, opts events.SubscribeOptions, h events.Handler, r *kgo.Record) {
	m := &events.Message{Subject: string(r.Key), Headers: map[string]string{}, Data: r.Value}
	for _, hd := range r.Headers {
		m.Headers[hd.Key] = string(hd.Value)
	}
	m.ID = m.Headers[events.HeaderMsgID]
	delete(m.Headers, events.HeaderMsgID)
	var err error
	for attempt := 1; attempt <= opts.MaxDeliver; attempt++ {
		m.Delivery = attempt
		if err = h(ctx, m); err == nil {
			return
		}
		if attempt < opts.MaxDeliver {
			select {
			case <-ctx.Done():
				return
			case <-time.After(opts.Delay(attempt)):
			}
		}
	}
	dlq := events.DLQSubject(ch.Stream)
	dead := &events.Message{Subject: m.Subject, ID: m.ID, Headers: map[string]string{events.HeaderError: err.Error()}, Data: m.Data}
	for k, v := range m.Headers {
		if k != events.HeaderError {
			dead.Headers[k] = v
		}
	}
	if e := b.ensureTopic(ctx, dlq); e == nil {
		_ = b.producer.ProduceSync(ctx, record(dlq, dead)).FirstErr()
	}
}

// Close stops every subscriber and the producer.
func (b *Backend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	for _, cancel := range b.subs {
		cancel()
	}
	b.mu.Unlock()
	b.producer.Close()
	return nil
}
