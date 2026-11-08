// Package kafka is an events.Backend over Apache Kafka (franz-go). It
// is its own Go module (github.com/iodesystems/gwag/v2/events/kafka),
// so the gwag root module carries no Kafka dependency.
//
// Default mapping (WithTopics changes every part of it):
//   - one topic per channel, named ch.Stream (EV_<pkg>_<Name>),
//     created with 3 partitions and replication 1 if missing;
//   - record key = subject (ordering per subject), value = the body
//     the Serde wrote (plain proto by default), headers = Gwag-Type,
//     Gwag-Channel, Gwag-Msg-Id and extras;
//   - group = Kafka consumer group "<topic>.<group>";
//   - a failed handler is retried in process with the backoff (the
//     partition waits), then the record goes to topic
//     gwag.dlq.<topic> with Gwag-Error and the offset is committed.
//     RetryTopic moves retries to a per-group retry topic instead.
//
// The producer is idempotent with acks=all (franz-go's defaults).
//
// No dedup: Kafka does not drop a record because its Gwag-Msg-Id was
// seen. Handlers must be idempotent.
//
// Stability: experimental
package kafka

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/iodesystems/gwag/v2/events"
)

// Headers this backend adds.
//
// Stability: experimental
const (
	// HeaderSubject carries the subject when the record key is not the
	// subject (Topic.Key is set).
	HeaderSubject = "Gwag-Subject"
	// HeaderAttempt carries the delivery attempt of a record on a
	// retry topic.
	HeaderAttempt = "Gwag-Attempt"
	// HeaderRetryAt carries when a retry record is due, in Unix
	// milliseconds.
	HeaderRetryAt = "Gwag-Retry-At"
)

// Topic is where one channel lives on Kafka. Zero fields take the
// defaults in DefaultTopic.
//
// Stability: experimental
type Topic struct {
	// Name is the topic. Default ch.Stream.
	Name string
	// Partitions for a topic the backend creates. Default
	// WithPartitions (3). A group spreads over at most this many
	// subscribers.
	Partitions int32
	// Replication for a topic the backend creates. Default
	// WithReplication (1); -1 is the broker default.
	Replication int16
	// Key returns the record key of a publish. Default: the subject.
	// When the key is not the subject, the subject rides in
	// HeaderSubject.
	Key func(subject string, m *events.Message) []byte
	// Subject returns the subject of a record without HeaderSubject or
	// Gwag-Channel, i.e. one a producer other than gwag wrote. Default:
	// the channel pattern when it has no wildcards, else the record key.
	Subject func(r *kgo.Record) string
	// Group returns the Kafka consumer group for a gwag group. Default
	// "<Name>.<group>".
	Group func(group string) string
	// DLQ is the dead-letter topic. Default "gwag.dlq.<Name>".
	DLQ string
	// Retry returns the retry topic of a group (RetryTopic mode).
	// Default "gwag.retry.<Name>.<group>". One per group, so a retry
	// reaches only the group that failed.
	Retry func(group string) string
}

// Mapper returns the Topic of a channel. Return a zero field to take
// its default.
//
// Stability: experimental
type Mapper func(ch events.Channel) Topic

// TopicTable returns a Mapper over a table keyed by channel key
// ("orders.v1.OrderEvents"). Channels not in the table take the
// defaults.
//
// Stability: experimental
func TopicTable(table map[string]Topic) Mapper {
	return func(ch events.Channel) Topic { return table[ch.Key] }
}

// Backend is the Kafka backend.
//
// Stability: experimental
type Backend struct {
	brokers     []string
	partitions  int32
	replication int16
	mapper      Mapper
	clientOpts  []kgo.Opt
	ownProducer bool
	producer    *kgo.Client
	admin       *kadm.Client

	topics sync.Map // topic -> struct{}

	mu     sync.Mutex
	subs   map[*kgo.Client]context.CancelFunc
	closed bool
}

// Option configures a Backend.
//
// Stability: experimental
type Option func(*Backend)

// WithPartitions sets the default partition count for topics the
// backend creates. Default 3.
//
// Stability: experimental
func WithPartitions(n int32) Option { return func(b *Backend) { b.partitions = n } }

// WithReplication sets the default replication factor for topics the
// backend creates. Default 1; -1 is the broker default.
//
// Stability: experimental
func WithReplication(n int16) Option { return func(b *Backend) { b.replication = n } }

// WithTopics sets the channel-to-topic mapper.
//
// Stability: experimental
func WithTopics(m Mapper) Option { return func(b *Backend) { b.mapper = m } }

// WithClientOpts adds franz-go options to every client the backend
// makes (the producer and each subscriber): SASL, TLS, client id,
// timeouts. They apply after the backend's own, so they win.
//
// Stability: experimental
func WithClientOpts(opts ...kgo.Opt) Option {
	return func(b *Backend) { b.clientOpts = append(b.clientOpts, opts...) }
}

// WithProducer makes the backend publish and administer topics through
// cl instead of a client of its own. Close does not close cl.
// Subscribers still make their own clients (a consumer group needs
// one each) from the brokers and WithClientOpts.
//
// Stability: experimental
func WithProducer(cl *kgo.Client) Option { return func(b *Backend) { b.producer = cl } }

// New connects to brokers.
//
// Stability: experimental
func New(brokers []string, opts ...Option) (*Backend, error) {
	b := &Backend{brokers: brokers, partitions: 3, replication: 1, subs: map[*kgo.Client]context.CancelFunc{}}
	for _, o := range opts {
		o(b)
	}
	if b.producer == nil {
		// Idempotent writes and acks=all are franz-go's defaults; set
		// acks anyway so a reader sees it.
		base := []kgo.Opt{kgo.SeedBrokers(brokers...), kgo.AllowAutoTopicCreation(), kgo.RequiredAcks(kgo.AllISRAcks())}
		p, err := kgo.NewClient(append(base, b.clientOpts...)...)
		if err != nil {
			return nil, err
		}
		b.producer = p
		b.ownProducer = true
	}
	b.admin = kadm.NewClient(b.producer)
	return b, nil
}

// DefaultTopic returns ch's topic with every default filled in.
//
// Stability: experimental
func (b *Backend) DefaultTopic(ch events.Channel) Topic { return b.fill(ch, Topic{}) }

// TopicOf returns ch's topic: the mapper's, defaults filled in.
//
// Stability: experimental
func (b *Backend) TopicOf(ch events.Channel) Topic {
	var t Topic
	if b.mapper != nil {
		t = b.mapper(ch)
	}
	return b.fill(ch, t)
}

// Topic implements events.Topics: the topic name, which a Serde may
// derive a schema subject from.
func (b *Backend) Topic(ch events.Channel) string { return b.TopicOf(ch).Name }

func (b *Backend) fill(ch events.Channel, t Topic) Topic {
	if t.Name == "" {
		t.Name = ch.Stream
	}
	if t.Partitions == 0 {
		t.Partitions = b.partitions
	}
	if t.Replication == 0 {
		t.Replication = b.replication
	}
	if t.Group == nil {
		name := t.Name
		t.Group = func(g string) string { return name + "." + g }
	}
	if t.DLQ == "" {
		t.DLQ = events.DLQSubject(t.Name)
	}
	if t.Retry == nil {
		name := t.Name
		t.Retry = func(g string) string { return "gwag.retry." + name + "." + g }
	}
	if t.Subject == nil {
		literal := len(ch.Pattern.Segments()) == 0
		pat := ch.Pattern.String()
		t.Subject = func(r *kgo.Record) string {
			if literal {
				return pat
			}
			return string(r.Key)
		}
	}
	return t
}

func (b *Backend) ensureTopic(ctx context.Context, topic string, partitions int32, replication int16) error {
	if _, ok := b.topics.Load(topic); ok {
		return nil
	}
	resp, err := b.admin.CreateTopic(ctx, partitions, replication, nil, topic)
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

func record(topic string, key []byte, m *events.Message) *kgo.Record {
	r := &kgo.Record{Topic: topic, Key: key, Value: m.Data}
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
	t := b.TopicOf(ch)
	if err := b.ensureTopic(ctx, t.Name, t.Partitions, t.Replication); err != nil {
		return err
	}
	key := []byte(m.Subject)
	if t.Key != nil {
		key = t.Key(m.Subject, m)
		if string(key) != m.Subject {
			m = withHeader(m, HeaderSubject, m.Subject)
		}
	}
	return b.producer.ProduceSync(ctx, record(t.Name, key, m)).FirstErr()
}

func withHeader(m *events.Message, k, v string) *events.Message {
	c := *m
	c.Headers = make(map[string]string, len(m.Headers)+1)
	for hk, hv := range m.Headers {
		c.Headers[hk] = hv
	}
	c.Headers[k] = v
	return &c
}

// Retry modes, selected per subscribe.
const (
	retryKey     = "kafka.retry"
	retryProcess = "process"
	retryTopic   = "topic"
)

// RetryInProcess retries a failed record in the subscriber, waiting the
// backoff between attempts. The partition waits too: later records on
// it are not delivered until this one is acked or dead-lettered. This
// is the default.
//
// Stability: experimental
func RetryInProcess() events.SubscribeOption { return events.BackendOption(retryKey, retryProcess) }

// RetryTopic moves a failed record to the group's retry topic
// (Topic.Retry) with HeaderAttempt and HeaderRetryAt, and commits it on
// the channel topic, so the partition moves on. A second consumer
// reads the retry topic, waits until each record is due, and delivers
// it again; after MaxDeliver failed deliveries the record goes to the
// DLQ. Order per key is not kept across a retry. A record due later
// holds up the retry records behind it (one retry topic per group, not
// one per delay).
//
// Stability: experimental
func RetryTopic() events.SubscribeOption { return events.BackendOption(retryKey, retryTopic) }

func retryMode(opts events.SubscribeOptions) string {
	if v, ok := opts.Backend[retryKey].(string); ok {
		return v
	}
	return retryProcess
}

// Subscribe implements events.Backend. It returns once the consumers
// hold a partition assignment.
func (b *Backend) Subscribe(ctx context.Context, ch events.Channel, group string, opts events.SubscribeOptions, h events.Handler) (func(), error) {
	t := b.TopicOf(ch)
	mode := retryMode(opts)
	if mode != retryProcess && mode != retryTopic {
		return nil, fmt.Errorf("kafka: unknown retry mode %q", mode)
	}
	if err := b.ensureTopic(ctx, t.Name, t.Partitions, t.Replication); err != nil {
		return nil, err
	}
	// Create the DLQ topic now. A reader of the DLQ that starts before
	// the first dead letter would otherwise wait for a metadata
	// refresh to find a topic created after it.
	if err := b.ensureTopic(ctx, t.DLQ, t.Partitions, t.Replication); err != nil {
		return nil, err
	}
	s := &sub{b: b, ch: ch, t: t, opts: opts, h: h, group: group}
	ctx, cancel := context.WithCancel(ctx)
	mainGroup := t.Group(group)
	if err := s.start(ctx, cancel, t.Name, mainGroup, opts.DeliverAll, s.handleMain); err != nil {
		cancel()
		return nil, err
	}
	if mode == retryTopic {
		s.retryTopic = t.Retry(group)
		if err := b.ensureTopic(ctx, s.retryTopic, t.Partitions, t.Replication); err != nil {
			cancel()
			return nil, err
		}
		if err := s.start(ctx, cancel, s.retryTopic, mainGroup+".retry", false, s.handleRetry); err != nil {
			cancel()
			return nil, err
		}
	}
	return cancel, nil
}

type sub struct {
	b          *Backend
	ch         events.Channel
	t          Topic
	opts       events.SubscribeOptions
	h          events.Handler
	group      string
	retryTopic string
}

// start seeds group on topic, starts a consumer that calls handle per
// record, and waits for its partition assignment. cancel stops it;
// Close calls cancel.
func (s *sub) start(ctx context.Context, cancel context.CancelFunc, topic, group string, deliverAll bool, handle func(context.Context, *kgo.Record)) error {
	b := s.b
	if err := b.seedGroup(ctx, topic, group, deliverAll); err != nil {
		return err
	}
	reset := kgo.NewOffset().AtEnd()
	if deliverAll {
		reset = kgo.NewOffset().AtStart()
	}
	assigned := make(chan struct{})
	var once sync.Once
	base := []kgo.Opt{
		kgo.SeedBrokers(b.brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(reset),
		kgo.DisableAutoCommit(),
		kgo.OnPartitionsAssigned(func(context.Context, *kgo.Client, map[string][]int32) {
			once.Do(func() { close(assigned) })
		}),
	}
	cl, err := kgo.NewClient(append(base, b.clientOpts...)...)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cl.Close()
		return errors.New("kafka: backend closed")
	}
	b.subs[cl] = cancel
	b.mu.Unlock()
	go s.consume(ctx, cl, handle)
	select {
	case <-assigned:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("kafka: group %s: no partition assignment after 30s", group)
	}
}

func (s *sub) consume(ctx context.Context, cl *kgo.Client, handle func(context.Context, *kgo.Record)) {
	b := s.b
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
		fetches.EachRecord(func(r *kgo.Record) {
			if ctx.Err() == nil {
				handle(ctx, r)
			}
		})
		if ctx.Err() != nil {
			return
		}
		_ = cl.CommitUncommittedOffsets(ctx)
	}
}

// message turns a record into the message handlers see.
func (s *sub) message(r *kgo.Record) *events.Message {
	m := &events.Message{Headers: map[string]string{}, Data: r.Value, Key: r.Key}
	for _, hd := range r.Headers {
		m.Headers[hd.Key] = string(hd.Value)
	}
	m.ID = m.Headers[events.HeaderMsgID]
	delete(m.Headers, events.HeaderMsgID)
	switch subj, ok := m.Headers[HeaderSubject]; {
	case ok:
		m.Subject = subj
		delete(m.Headers, HeaderSubject)
	case m.Headers[events.HeaderChannel] != "":
		m.Subject = string(r.Key)
	default:
		m.Subject = s.t.Subject(r)
	}
	return m
}

func (s *sub) handleMain(ctx context.Context, r *kgo.Record) {
	m := s.message(r)
	if s.retryTopic != "" {
		m.Delivery = 1
		if err := s.h(ctx, m); err != nil {
			s.fail(ctx, r, m, err)
		}
		return
	}
	var err error
	for attempt := 1; attempt <= s.opts.MaxDeliver; attempt++ {
		m.Delivery = attempt
		if err = s.h(ctx, m); err == nil {
			return
		}
		if attempt < s.opts.MaxDeliver {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.opts.Delay(attempt)):
			}
		}
	}
	s.deadLetter(ctx, r, m, err)
}

func (s *sub) handleRetry(ctx context.Context, r *kgo.Record) {
	m := s.message(r)
	attempt, _ := strconv.Atoi(m.Headers[HeaderAttempt])
	if attempt < 2 {
		attempt = 2
	}
	if at, err := strconv.ParseInt(m.Headers[HeaderRetryAt], 10, 64); err == nil {
		if wait := time.Until(time.UnixMilli(at)); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
	delete(m.Headers, HeaderAttempt)
	delete(m.Headers, HeaderRetryAt)
	m.Delivery = attempt
	if err := s.h(ctx, m); err != nil {
		s.fail(ctx, r, m, err)
	}
}

// fail sends a record that failed delivery m.Delivery to the retry
// topic, or to the DLQ after the last attempt. It returns once the
// record is written, so the offset is committed only after that.
func (s *sub) fail(ctx context.Context, r *kgo.Record, m *events.Message, err error) {
	if m.Delivery >= s.opts.MaxDeliver {
		s.deadLetter(ctx, r, m, err)
		return
	}
	next := withHeader(m, HeaderAttempt, strconv.Itoa(m.Delivery+1))
	next.Headers[HeaderRetryAt] = strconv.FormatInt(time.Now().Add(s.opts.Delay(m.Delivery)).UnixMilli(), 10)
	s.produce(ctx, s.rewrite(r, s.retryTopic, next))
}

func (s *sub) deadLetter(ctx context.Context, r *kgo.Record, m *events.Message, err error) {
	dead := withHeader(m, events.HeaderError, err.Error())
	delete(dead.Headers, HeaderAttempt)
	delete(dead.Headers, HeaderRetryAt)
	if e := s.b.ensureTopic(ctx, s.t.DLQ, s.t.Partitions, s.t.Replication); e == nil {
		_ = s.b.producer.ProduceSync(ctx, s.rewrite(r, s.t.DLQ, dead)).FirstErr()
	}
}

// rewrite returns r for topic with m's headers, keeping r's key and
// raw value (the Serde's bytes, not the decoded copy) and the subject.
func (s *sub) rewrite(r *kgo.Record, topic string, m *events.Message) *kgo.Record {
	raw := *m
	raw.Data = r.Value
	if string(r.Key) != m.Subject {
		raw = *withHeader(&raw, HeaderSubject, m.Subject)
	}
	return record(topic, r.Key, &raw)
}

// produce writes rec, retrying until it is written or ctx ends: a
// retry record that is lost would drop the message.
func (s *sub) produce(ctx context.Context, rec *kgo.Record) {
	for {
		err := s.b.producer.ProduceSync(ctx, rec).FirstErr()
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// Close stops every subscriber and, unless it came from WithProducer,
// the producer.
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
	if b.ownProducer {
		b.producer.Close()
	}
	return nil
}
