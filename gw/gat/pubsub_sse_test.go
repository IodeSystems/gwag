package gat

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscribeSSE_StreamsEvents(t *testing.T) {
	g, _ := New()
	defer g.Close()
	srv := httptest.NewServer(g.subscribeSSEHandler())
	defer srv.Close()

	// http.Get returns once response headers arrive — by then the
	// handler has registered the subscription and flushed.
	resp, err := http.Get(srv.URL + "?channel=room.>")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q; want text/event-stream", ct)
	}

	g.PubSub().Publish("room.42", []byte("hi"))

	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			s, err := r.ReadString('\n')
			if err != nil {
				got <- result{err: err}
				return
			}
			if strings.HasPrefix(s, "data: ") {
				got <- result{line: s}
				return
			}
		}
	}()

	select {
	case res := <-got:
		if res.err != nil {
			t.Fatalf("read SSE: %v", res.err)
		}
		// Payload []byte marshals as base64: "hi" → "aGk=".
		if !strings.Contains(res.line, `"channel":"room.42"`) ||
			!strings.Contains(res.line, `"payload":"aGk="`) {
			t.Errorf("SSE data line = %q", res.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestSubscribeSSE_MissingChannelRejected(t *testing.T) {
	g, _ := New()
	defer g.Close()
	srv := httptest.NewServer(g.subscribeSSEHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for missing channel", resp.StatusCode)
	}
}
