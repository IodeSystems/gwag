package kafka_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/eventstest"
	"github.com/iodesystems/gwag/v2/events/kafka"
)

// brokers returns KAFKA_BROKERS or skips.
// examples/events/go-kafka/test.sh starts apache/kafka in docker and
// sets it.
func brokers(t *testing.T) []string {
	v := os.Getenv("KAFKA_BROKERS")
	if v == "" {
		t.Skip("KAFKA_BROKERS not set; run examples/events/go-kafka/test.sh")
	}
	return strings.Split(v, ",")
}

// readTopic waits for n records on topic, from its start.
func readTopic(t *testing.T, bs []string, topic string, n int) []*kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(bs...), kgo.ConsumeTopics(topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		f := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("%s: got %d records, want %d", topic, len(out), n)
		}
		f.EachRecord(func(r *kgo.Record) { out = append(out, r) })
	}
	return out
}

func harness(bs []string, opts ...events.SubscribeOption) eventstest.Harness {
	return eventstest.Harness{
		New: func(t *testing.T) events.Backend {
			b, err := kafka.New(bs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close() })
			return b
		},
		DeadLetters: func(t *testing.T, _ events.Backend, stream string, n int) []*events.Message {
			var out []*events.Message
			for _, r := range readTopic(t, bs, events.DLQSubject(stream), n) {
				m := &events.Message{Subject: r.Topic, Headers: map[string]string{}, Data: r.Value}
				for _, h := range r.Headers {
					m.Headers[h.Key] = string(h.Value)
				}
				out = append(out, m)
			}
			return out
		},
		// Offsets of a new group resolve after assignment.
		Settle:           time.Second,
		NoDedup:          true,
		SubscribeOptions: opts,
	}
}

func TestBackend(t *testing.T) {
	eventstest.Run(t, harness(brokers(t)))
}

func TestBackendRetryTopic(t *testing.T) {
	eventstest.Run(t, harness(brokers(t), kafka.RetryTopic()))
}

// With RetryTopic a failing record does not hold up the next one on
// its partition, and its retries carry the attempt number.
func TestRetryTopicDoesNotBlockPartition(t *testing.T) {
	bs := brokers(t)
	b, err := kafka.New(bs, kafka.WithPartitions(1))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c := events.NewCore(b)
	ch := eventstest.Channel(t)
	var mu sync.Mutex
	var seen []string
	done := make(chan struct{})
	stop, err := c.Subscribe(context.Background(), ch, "rt", func(ctx context.Context, segs []string, m *events.Message) error {
		var v wrapperspb.StringValue
		_ = proto.Unmarshal(m.Data, &v)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf("%s#%d", v.Value, m.Delivery))
		if v.Value == "bad" && m.Delivery < 3 {
			return errors.New("not yet")
		}
		if v.Value == "bad" {
			close(done)
		}
		return nil
	}, kafka.RetryTopic(), events.Backoff(300*time.Millisecond), events.MaxDeliver(3))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, body := range []string{"bad", "good"} {
		if err := c.Publish(context.Background(), ch, []string{"same"}, wrapperspb.String(body)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("no third delivery of bad")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen, " "); got != "bad#1 good#1 bad#2 bad#3" {
		t.Fatalf("deliveries %q, want %q", got, "bad#1 good#1 bad#2 bad#3")
	}
}

// The mapper names the topic, key, group and DLQ. A record key that is
// not the subject sends the subject in Gwag-Subject. A record without
// gwag headers on a channel whose pattern has no wildcards gets the
// pattern as its subject, and its key in Message.Key.
func TestMapper(t *testing.T) {
	bs := brokers(t)
	ch := eventstest.Channel(t)
	lit := events.MustChannel(ch.Key+"Lit", "corp.orders")
	suffix := events.NewID()[:8]
	topic, litTopic := "map-"+suffix, "corp-orders-"+suffix
	b, err := kafka.New(bs, kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
		ch.Key: {
			Name: topic,
			Key:  func(subject string, m *events.Message) []byte { return []byte("k-" + subject) },
			DLQ:  topic + ".dead",
		},
		lit.Key: {Name: litTopic, Group: func(g string) string { return "corp-" + g + "-" + suffix }},
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := b.Topic(ch); got != topic {
		t.Fatalf("Topic() = %q", got)
	}
	c := events.NewCore(b)
	got := make(chan *events.Message, 4)
	for _, x := range []events.Channel{ch, lit} {
		stop, err := c.Subscribe(context.Background(), x, "m", func(ctx context.Context, segs []string, m *events.Message) error {
			got <- m
			if string(m.Key) == "fail" {
				return errors.New("dead")
			}
			return nil
		}, events.MaxDeliver(1))
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
	}
	if err := c.Publish(context.Background(), ch, []string{"a1"}, wrapperspb.String("x")); err != nil {
		t.Fatal(err)
	}
	m := <-got
	if wantSubj := strings.Replace(ch.Pattern.String(), "{id}", "a1", 1); m.Subject != wantSubj || string(m.Key) != "k-"+wantSubj {
		t.Fatalf("subject %q key %q", m.Subject, m.Key)
	}
	if _, ok := m.Headers[kafka.HeaderSubject]; ok {
		t.Fatalf("handler sees %s: %v", kafka.HeaderSubject, m.Headers)
	}
	// A record from a producer that is not gwag: no headers.
	raw, _ := proto.Marshal(wrapperspb.String("corp"))
	p, err := kgo.NewClient(kgo.SeedBrokers(bs...))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.ProduceSync(context.Background(), &kgo.Record{Topic: litTopic, Key: []byte("fail"), Value: raw}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	select {
	case m = <-got:
	case <-time.After(20 * time.Second):
		t.Fatal("no delivery of the headerless record")
	}
	if m.Subject != "corp.orders" || string(m.Key) != "fail" || m.Type() != "" {
		t.Fatalf("subject %q key %q type %q", m.Subject, m.Key, m.Type())
	}
	dead := readTopic(t, bs, events.DLQSubject(litTopic), 1)[0]
	if string(dead.Key) != "fail" {
		t.Fatalf("dead letter key %q", dead.Key)
	}
	hs := map[string]string{}
	for _, h := range dead.Headers {
		hs[h.Key] = string(h.Value)
	}
	if hs[events.HeaderError] != "dead" || hs[kafka.HeaderSubject] != "corp.orders" {
		t.Fatalf("dead letter headers %v", hs)
	}
}
