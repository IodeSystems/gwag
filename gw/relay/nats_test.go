package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// TestNatsSource_DeliversPublish is the real end-to-end: an embedded NATS server,
// NatsSource wired into the relay, a WS client subscribing with a cap, and a
// genuine nc.Publish on the channel subject — assert the published bytes arrive
// as a data frame. This is the "messages actually flow: publish → bus → relay →
// subscriber" verification (the fake-source tests don't exercise NATS).
func TestNatsSource_DeliversPublish(t *testing.T) {
	ns := natstest.RunServer(&server.Options{Host: "127.0.0.1", Port: -1})
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	secret := []byte("k")
	srv := httptest.NewServer(New(secret, NatsSource(nc)).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	cp, _ := Sign(secret, Cap{Channel: "events.drive.1"})
	writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: cp})
	if ack := readFrame(t, c); ack.Op != "ack" || ack.ID != "1" {
		t.Fatalf("want ack/1, got %+v", ack)
	}

	// ack is sent after NatsSource ran nc.Subscribe; flush so the SUB is
	// server-side before we publish (same connection → ordered, but flush is
	// belt-and-suspenders against async propagation).
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	want := `{"type":"donation","fundraiserId":"1"}`
	if err := nc.Publish("events.drive.1", []byte(want)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_ = nc.Flush()

	f := readFrame(t, c)
	if f.Op != "data" || f.ID != "1" || string(f.Payload) != want {
		t.Fatalf("want data/1 %s, got %+v (%s)", want, f, f.Payload)
	}

	// And a publish on a different subject must NOT arrive (channel isolation).
	_ = nc.Publish("events.drive.2", []byte(`{"x":1}`))
	_ = nc.Flush()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var stray outFrame
	if err := wsjson.Read(ctx, c, &stray); err == nil {
		t.Fatalf("received a frame for an unsubscribed subject: %+v", stray)
	}
}
