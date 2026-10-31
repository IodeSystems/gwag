package relay

import (
	"context"
	"errors"
	"net/http"
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

	// Distinct channels so the count reflects all subs (same-channel subs would
	// coalesce to one upstream — see TestRelay_CoalescesSameChannel).
	for _, ch := range []string{"chA", "chB"} {
		cp, _ := Sign(secret, Cap{Channel: ch})
		writeFrame(t, c, inFrame{Op: "sub", ID: ch, Token: cp})
		readFrame(t, c)
	}
	waitFor(t, func() bool { return src.count("chA") == 1 && src.count("chB") == 1 })

	done()
	waitFor(t, func() bool { return src.count("chA") == 0 && src.count("chB") == 0 })
}

// TestRelay_CoalescesSameChannel: two WS clients on ONE relay, same channel,
// share a single upstream subscription; both receive each event; the upstream
// drops only when the last client leaves. (Coalescing is per relay instance —
// across a pool each relay holds one upstream sub per channel for its locals.)
func TestRelay_CoalescesSameChannel(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	srv := httptest.NewServer(New(secret, src).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(context.Background(), url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}

	c1, c2 := dial(), dial()
	defer func() { _ = c1.CloseNow() }()
	cp, _ := Sign(secret, Cap{Channel: "hot"})
	writeFrame(t, c1, inFrame{Op: "sub", ID: "1", Token: cp})
	readFrame(t, c1)
	writeFrame(t, c2, inFrame{Op: "sub", ID: "1", Token: cp})
	readFrame(t, c2)

	// One upstream subscription serves both clients.
	waitFor(t, func() bool { return src.count("hot") == 1 })

	src.publish("hot", []byte(`{"v":1}`))
	if f := readFrame(t, c1); string(f.Payload) != `{"v":1}` {
		t.Fatalf("c1 payload = %s", f.Payload)
	}
	if f := readFrame(t, c2); string(f.Payload) != `{"v":1}` {
		t.Fatalf("c2 payload = %s", f.Payload)
	}

	// One client leaves → upstream stays (the other still wants it).
	_ = c2.CloseNow()
	waitFor(t, func() bool { return src.count("hot") == 1 })
	// Last client leaves → upstream drops.
	_ = c1.CloseNow()
	waitFor(t, func() bool { return src.count("hot") == 0 })
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

func TestRelay_PingPong(t *testing.T) {
	c, done := dialRelay(t, []byte("k"), newFakeSource())
	defer done()
	writeFrame(t, c, inFrame{Op: "ping", ID: "hb"})
	if f := readFrame(t, c); f.Op != "pong" || f.ID != "hb" {
		t.Fatalf("want pong/hb, got %+v", f)
	}
}

// TestRelay_Compression: WithCompression negotiates permessage-deflate when the client
// offers it, and frames still arrive intact; without the option nothing is negotiated.
func TestRelay_Compression(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want bool
	}{
		{"default off", nil, false},
		{"WithCompression", []Option{WithCompression()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := []byte("k")
			src := newFakeSource()
			srv := httptest.NewServer(New(secret, src, tc.opts...).Handler())
			defer srv.Close()
			url := "ws" + strings.TrimPrefix(srv.URL, "http")
			c, resp, err := websocket.Dial(context.Background(), url,
				&websocket.DialOptions{CompressionMode: websocket.CompressionNoContextTakeover})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.CloseNow() }()
			got := strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate")
			if got != tc.want {
				t.Fatalf("permessage-deflate negotiated = %v, want %v (%q)", got, tc.want, resp.Header.Get("Sec-WebSocket-Extensions"))
			}
			tok, _ := Sign(secret, Cap{Channel: "ch"})
			writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: tok})
			if ack := readFrame(t, c); ack.Op != "ack" {
				t.Fatalf("want ack, got %+v", ack)
			}
			waitFor(t, func() bool { return src.count("ch") == 1 })
			big := `{"rows":"` + strings.Repeat("abcdefgh", 1000) + `"}` // over the 512 B threshold
			src.publish("ch", []byte(big))
			if f := readFrame(t, c); f.Op != "data" || string(f.Payload) != big {
				t.Fatalf("payload mangled: op=%s len=%d", f.Op, len(f.Payload))
			}
		})
	}
}

func TestRelay_Shutdown(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	r := New(secret, src)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	tok, _ := Sign(secret, Cap{Channel: "ch"})

	var conns []*websocket.Conn
	for range 3 {
		c, _, err := websocket.Dial(context.Background(), url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = c.CloseNow() }()
		writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: tok})
		if ack := readFrame(t, c); ack.Op != "ack" {
			t.Fatalf("want ack, got %+v", ack)
		}
		conns = append(conns, c)
	}

	// A client reads concurrently, as a browser does: that is what takes the
	// close frame.
	codes := make(chan websocket.CloseError, len(conns))
	for _, c := range conns {
		go func() {
			_, _, err := c.Read(context.Background())
			var ce websocket.CloseError
			errors.As(err, &ce)
			codes <- ce
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for range conns {
		if ce := <-codes; ce.Code != websocket.StatusGoingAway || ce.Reason != "server restarting" {
			t.Errorf("close = %+v", ce)
		}
	}
	if n := src.count("ch"); n != 0 {
		t.Errorf("%d source subs left after Shutdown", n)
	}

	if _, resp, err := websocket.Dial(context.Background(), url, nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial after Shutdown: err=%v resp=%+v", err, resp)
	}
	if err := r.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

// A client that never reads must not hold Shutdown past its context.
func TestRelay_ShutdownHonorsContext(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	r := New(secret, src)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	tok, _ := Sign(secret, Cap{Channel: "ch"})
	writeFrame(t, c, inFrame{Op: "sub", ID: "1", Token: tok})
	if ack := readFrame(t, c); ack.Op != "ack" {
		t.Fatalf("want ack, got %+v", ack)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := r.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Shutdown took %s", d)
	}
	waitFor(t, func() bool { return src.count("ch") == 0 })
}

func TestRelay_NoConnsLeftTracked(t *testing.T) {
	r := New([]byte("k"), newFakeSource())
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	writeFrame(t, c, inFrame{Op: "ping", ID: "hb"})
	if f := readFrame(t, c); f.Op != "pong" {
		t.Fatalf("want pong, got %+v", f)
	}
	_ = c.CloseNow()
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.conns) == 0
	})
}
