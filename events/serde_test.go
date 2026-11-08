package events_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/memory"
)

// prefixSerde frames the body with "X:" and, on decode, names the type
// when the header is missing, as a registry serde does.
type prefixSerde struct{ topics []string }

func (s *prefixSerde) Encode(_ context.Context, _ events.Channel, topic string, m proto.Message, out *events.Message) error {
	s.topics = append(s.topics, topic)
	b, err := proto.Marshal(m)
	out.Data = append([]byte("X:"), b...)
	out.Headers["X-Framed"] = "1"
	return err
}

func (s *prefixSerde) Decode(_ context.Context, _ events.Channel, _ string, in *events.Message) error {
	if !bytes.HasPrefix(in.Data, []byte("X:")) {
		return errors.New("no frame")
	}
	in.Data = in.Data[2:]
	if in.Headers[events.HeaderType] == "" {
		in.Headers[events.HeaderType] = "google.protobuf.StringValue"
	}
	return nil
}

func TestSerdeRoundTrip(t *testing.T) {
	b := memory.New()
	defer b.Close()
	s := &prefixSerde{}
	c := events.NewCore(b, events.WithSerde(s))
	ch := events.MustChannel("t.v1.S", "t.s.{id}")
	got := make(chan *events.Message, 1)
	stop, err := c.Subscribe(context.Background(), ch, "g", func(ctx context.Context, segs []string, m *events.Message) error {
		got <- m
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := c.Publish(context.Background(), ch, []string{"a"}, wrapperspb.String("hi")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		var v wrapperspb.StringValue
		if err := proto.Unmarshal(m.Data, &v); err != nil || v.Value != "hi" {
			t.Fatalf("body %q: %v", m.Data, err)
		}
		if m.Headers["X-Framed"] != "1" {
			t.Fatalf("headers %v", m.Headers)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
	if len(s.topics) != 1 || s.topics[0] != ch.Stream {
		t.Fatalf("topics %v", s.topics)
	}
}

// A failed message is dead-lettered with the original
// (framed) body, not the decoded copy.
func TestSerdeDeadLettersRaw(t *testing.T) {
	b := memory.New()
	defer b.Close()
	ch := events.MustChannel("t.v1.D", "t.d.{id}")
	c := events.NewCore(b, events.WithSerde(&prefixSerde{}))
	stop, err := c.Subscribe(context.Background(), ch, "g", func(context.Context, []string, *events.Message) error {
		return errors.New("handler fails")
	}, events.MaxDeliver(1))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := c.Publish(context.Background(), ch, []string{"a"}, wrapperspb.String("hi")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(b.DeadLetters()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no dead letter")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d := b.DeadLetters()[0]; !bytes.HasPrefix(d.Data, []byte("X:")) {
		t.Fatalf("dead letter body %q, want the framed original", d.Data)
	}
}
