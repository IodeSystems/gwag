package kafka_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/eventstest"
	"github.com/iodesystems/gwag/v2/events/kafka"
)

// brokers returns KAFKA_BROKERS or skips. ../test.sh starts
// apache/kafka in docker and sets it.
func brokers(t *testing.T) []string {
	v := os.Getenv("KAFKA_BROKERS")
	if v == "" {
		t.Skip("KAFKA_BROKERS not set; run ../test.sh")
	}
	return strings.Split(v, ",")
}

func TestBackend(t *testing.T) {
	bs := brokers(t)
	eventstest.Run(t, eventstest.Harness{
		New: func(t *testing.T) events.Backend {
			b, err := kafka.New(bs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close() })
			return b
		},
		DeadLetters: func(t *testing.T, _ events.Backend, stream string, n int) []*events.Message {
			cl, err := kgo.NewClient(kgo.SeedBrokers(bs...), kgo.ConsumeTopics(events.DLQSubject(stream)),
				kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
			if err != nil {
				t.Fatal(err)
			}
			defer cl.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var out []*events.Message
			for len(out) < n {
				f := cl.PollFetches(ctx)
				if ctx.Err() != nil {
					t.Fatalf("got %d dead letters, want %d", len(out), n)
				}
				f.EachRecord(func(r *kgo.Record) {
					m := &events.Message{Subject: r.Topic, Headers: map[string]string{}, Data: r.Value}
					for _, h := range r.Headers {
						m.Headers[h.Key] = string(h.Value)
					}
					out = append(out, m)
				})
			}
			return out
		},
		// Offsets of a new group resolve after assignment.
		Settle:  time.Second,
		NoDedup: true,
	})
}
