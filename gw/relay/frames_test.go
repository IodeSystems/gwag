package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"nhooyr.io/websocket"
)

func bigPayload(fill string) json.RawMessage {
	return json.RawMessage(`{"rows":"` + strings.Repeat(fill, compressThreshold) + `"}`)
}

func TestFrameCache(t *testing.T) {
	fc := newFrameCache()
	fc.acquire("ch")
	fc.acquire("ch")
	data := func(id string, p json.RawMessage) outFrame { return outFrame{Op: "data", ID: id, Payload: p} }

	a := bigPayload("a")
	first := fc.frame("ch", data("1", a))
	if first == nil {
		t.Fatal("no frame")
	}
	// Same event, same id, a copy of the bytes: shared.
	if got := fc.frame("ch", data("1", append(json.RawMessage(nil), a...))); got != first {
		t.Error("same event and id did not share a frame")
	}
	if got := fc.frame("ch", data("2", a)); got == first || got == nil {
		t.Error("a different id must get its own frame")
	}
	if got := fc.frame("ch", data("1", bigPayload("b"))); got == first || got == nil {
		t.Error("a new event must get its own frame")
	}
	for i := 0; i < maxSharedIDs-1; i++ { // id "1" holds a slot
		if fc.frame("ch", data(fmt.Sprint("id", i), bigPayload("b"))) == nil {
			t.Fatalf("id %d not prepared", i)
		}
	}
	if fc.frame("ch", data("past", bigPayload("b"))) != nil {
		t.Error("prepared past maxSharedIDs")
	}
	if fc.frame("ch", data("1", bigPayload("b"))) == nil {
		t.Error("a known id lost its frame")
	}
	if got := fc.frame("ch", data("1", json.RawMessage(`{}`))); got != nil {
		t.Error("a small frame was prepared")
	}
	if got := fc.frame("other", data("1", a)); got != nil {
		t.Error("an unacquired channel was prepared")
	}

	fc.release("ch")
	if fc.frame("ch", data("1", a)) == nil {
		t.Error("released while a ref is held")
	}
	fc.release("ch")
	if len(fc.channels) != 0 {
		t.Errorf("entry left behind: %v", fc.channels)
	}

	var off *frameCache
	off.acquire("ch")
	off.release("ch")
	if off.frame("ch", data("1", a)) != nil {
		t.Error("nil cache returned a frame")
	}
}

// A Source may reuse its buffer between events: the second event must not match
// the first by way of the shared slice.
func TestFrameCache_SourceReusesBuffer(t *testing.T) {
	fc := newFrameCache()
	fc.acquire("ch")
	buf := append(json.RawMessage(nil), bigPayload("a")...)
	first := fc.frame("ch", outFrame{Op: "data", ID: "1", Payload: buf})
	copy(buf, bigPayload("b"))
	if fc.frame("ch", outFrame{Op: "data", ID: "1", Payload: buf}) == first {
		t.Error("stale frame served")
	}
}

// One compressing and one plain client on the same channel and sub id share a
// PreparedMessage; each gets the frame in the form it negotiated.
func TestRelay_CompressionMixedClients(t *testing.T) {
	secret := []byte("k")
	src := newFakeSource()
	srv := httptest.NewServer(New(secret, src, WithCompression()).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	tok, _ := Sign(secret, Cap{Channel: "ch"})

	var conns []*websocket.Conn
	for _, mode := range []websocket.CompressionMode{websocket.CompressionNoContextTakeover, websocket.CompressionDisabled} {
		c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{CompressionMode: mode})
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
	want := string(bigPayload("x"))
	src.publish("ch", []byte(want))
	for i, c := range conns {
		if f := readFrame(t, c); f.Op != "data" || string(f.Payload) != want {
			t.Fatalf("client %d: op=%s len=%d", i, f.Op, len(f.Payload))
		}
	}
}
