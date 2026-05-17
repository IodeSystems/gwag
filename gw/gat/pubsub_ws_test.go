package gat

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// wsURL turns an httptest server URL into a ws:// subscribe URL.
func wsURL(srv *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + SubscribePath + "?" + query
}

// dialWS connects to the subscribe endpoint; t.Fatal on handshake
// failure. The 100ms settle lets the handler reach Subscribe before
// the caller publishes.
func dialWS(t *testing.T, srv *httptest.Server, query string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, wsURL(srv, query), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	time.Sleep(100 * time.Millisecond)
	return conn, ctx
}

func TestSubscribeWS_StreamsEvents(t *testing.T) {
	g, _ := New()
	defer g.Close()
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()

	conn, ctx := dialWS(t, srv, "channel=room.>")
	g.PubSub().Publish("room.42", []byte("hi"))

	mt, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if mt != websocket.MessageText {
		t.Errorf("frame type = %v; want text", mt)
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	if ev.Channel != "room.42" || string(ev.Payload) != "hi" {
		t.Errorf("event = %+v; want room.42/hi", ev)
	}
}

func TestSubscribeWS_OpenWhenNoAuthConfigured(t *testing.T) {
	g, _ := New()
	defer g.Close()
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()
	// No EnableSubscribeAuth → dial without a token must succeed.
	conn, _ := dialWS(t, srv, "channel=c")
	_ = conn
}

func TestSubscribeWS_AuthRejectsMissingToken(t *testing.T) {
	g, _ := New()
	defer g.Close()
	g.EnableSubscribeAuth([]byte("sub-secret"))
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv, "channel=c"), nil)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("dial without a token should be rejected")
	}
}

func TestSubscribeWS_AuthRejectsBadToken(t *testing.T) {
	g, _ := New()
	defer g.Close()
	g.EnableSubscribeAuth([]byte("sub-secret"))
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()

	// Token minted with the wrong key.
	tok, ts := SignSubscribeToken([]byte("wrong-key"), "c")
	q := "channel=c&token=" + tok + "&ts=" + strconv.FormatInt(ts, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv, q), nil)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("dial with a bad token should be rejected")
	}
}

func TestSubscribeWS_AuthAcceptsValidToken(t *testing.T) {
	secret := []byte("sub-secret")
	g, _ := New()
	defer g.Close()
	g.EnableSubscribeAuth(secret)
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()

	tok, ts := SignSubscribeToken(secret, "room.>")
	q := "channel=room.>&token=" + tok + "&ts=" + strconv.FormatInt(ts, 10)
	conn, ctx := dialWS(t, srv, q)

	g.PubSub().Publish("room.7", []byte("ok"))
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var ev Event
	json.Unmarshal(data, &ev)
	if ev.Channel != "room.7" {
		t.Errorf("event channel = %q; want room.7", ev.Channel)
	}
}

func TestSubscribeWS_AuthRejectsExpiredToken(t *testing.T) {
	secret := []byte("sub-secret")
	g, _ := New()
	defer g.Close()
	g.EnableSubscribeAuth(secret)
	srv := httptest.NewServer(g.subscribeWSHandler())
	defer srv.Close()

	// A token timestamped well outside the skew window.
	oldTS := time.Now().Add(-2 * subscribeTokenSkew).Unix()
	tok := computeSubscribeToken(secret, "c", oldTS)
	q := "channel=c&token=" + tok + "&ts=" + strconv.FormatInt(oldTS, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv, q), nil)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("dial with an expired token should be rejected")
	}
}
