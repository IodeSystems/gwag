package relay

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// fakeSource is an in-memory bus: publish(ch, data) fans to every live
// subscriber of ch.
type fakeSource struct {
	mu   sync.Mutex
	next int
	subs map[string]map[int]func([]byte)
}

func newFakeSource() *fakeSource { return &fakeSource{subs: map[string]map[int]func([]byte){}} }

func (f *fakeSource) Subscribe(ch string, deliver func([]byte)) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs[ch] == nil {
		f.subs[ch] = map[int]func([]byte){}
	}
	id := f.next
	f.next++
	f.subs[ch][id] = deliver
	return func() {
		f.mu.Lock()
		delete(f.subs[ch], id)
		f.mu.Unlock()
	}, nil
}

func (f *fakeSource) publish(ch string, data []byte) {
	f.mu.Lock()
	ds := make([]func([]byte), 0, len(f.subs[ch]))
	for _, d := range f.subs[ch] {
		ds = append(ds, d)
	}
	f.mu.Unlock()
	for _, d := range ds {
		d(data)
	}
}

func (f *fakeSource) count(ch string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[ch])
}

func dialRelay(t *testing.T, secret []byte, src Source) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(New(secret, src).Handler())
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return c, func() { _ = c.CloseNow(); srv.Close() }
}

func readFrame(t *testing.T, c *websocket.Conn) outFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var f outFrame
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

func writeFrame(t *testing.T, c *websocket.Conn, f inFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, f); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func TestRelay_SubReceivesAndIsolates(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	c, done := dialRelay(t, secret, src)
	defer done()

	capA, _ := Sign(secret, Cap{Channel: "chA"})
	writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: capA})
	if ack := readFrame(t, c); ack.Op != "ack" || ack.ID != "1" {
		t.Fatalf("want ack/1, got %+v", ack)
	}
	waitFor(t, func() bool { return src.count("chA") == 1 })
	src.publish("chB", []byte(`{"x":2}`)) // different channel — must NOT arrive
	src.publish("chA", []byte(`{"x":1}`))

	f := readFrame(t, c)
	if f.Op != "data" || f.ID != "1" || string(f.Payload) != `{"x":1}` {
		t.Fatalf("want data/1 {x:1}, got %+v (%s)", f, f.Payload)
	}
}

func TestRelay_BadCapErrsNoSub(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	c, done := dialRelay(t, secret, src)
	defer done()

	writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: "garbage"})
	if f := readFrame(t, c); f.Op != "err" || f.ID != "1" {
		t.Fatalf("want err/1, got %+v", f)
	}
	if src.count("chA") != 0 {
		t.Fatalf("a bad cap must not create a Source subscription")
	}
}

func TestRelay_Unsub(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	c, done := dialRelay(t, secret, src)
	defer done()

	cp, _ := Sign(secret, Cap{Channel: "chA"})
	writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: cp})
	readFrame(t, c) // ack
	waitFor(t, func() bool { return src.count("chA") == 1 })

	writeFrame(t, c, inFrame{Op: "unsub", ID: "1"})
	waitFor(t, func() bool { return src.count("chA") == 0 })
}

func TestRelay_DisconnectCancelsAllSubs(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	c, done := dialRelay(t, secret, src)

	for _, id := range []string{"1", "2"} {
		cp, _ := Sign(secret, Cap{Channel: "chA"})
		writeFrame(t, c, inFrame{Op: "sub", ID: id, Token: cp})
		readFrame(t, c)
	}
	waitFor(t, func() bool { return src.count("chA") == 2 })

	done()
	waitFor(t, func() bool { return src.count("chA") == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout")
}
