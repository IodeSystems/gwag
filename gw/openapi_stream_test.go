package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"nhooyr.io/websocket"
)

const tickStreamSpec = `{
  "openapi": "3.2.0",
  "info": {"title": "ticks", "version": "1.0.0"},
  "paths": {
    "/sse": {"get": {"operationId": "sse",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"text/event-stream": {"itemSchema": {
        "type": "object", "required": ["data"],
        "properties": {"data": {"type": "string", "contentMediaType": "application/json",
          "contentSchema": {"$ref": "#/components/schemas/Tick"}}}}}}}}}},
    "/jsonl": {"get": {"operationId": "jsonl",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/seq": {"get": {"operationId": "seq",
      "parameters": [{"name": "n", "in": "query", "required": true, "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"application/json-seq": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/forever": {"get": {"operationId": "forever",
      "responses": {"200": {"description": "ok", "content": {"application/x-ndjson": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/denied": {"get": {"operationId": "denied",
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/bad": {"get": {"operationId": "bad",
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}}
  },
  "components": {"schemas": {"Tick": {"type": "object", "required": ["n"],
    "properties": {"n": {"type": "integer"}}}}}
}`

type tickUpstreamState struct {
	gone   chan struct{} // one send per /forever request that ended
	accept chan string   // Accept header of each request
}

// tickUpstream streams ticks 1..n in each path's framing with a flush
// per tick. /forever streams until the client leaves; /denied
// answers 403; /bad sends one tick then a line that is not JSON.
func tickUpstream(t *testing.T) (*httptest.Server, *tickUpstreamState) {
	t.Helper()
	st := &tickUpstreamState{gone: make(chan struct{}, 4), accept: make(chan string, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case st.accept <- r.Header.Get("Accept"):
		default:
		}
		fl := w.(http.Flusher)
		frame := map[string]string{
			"/sse":     "data: {\"n\":%d}\n\n",
			"/jsonl":   "{\"n\":%d}\n",
			"/seq":     "\x1e{\"n\":%d}\n",
			"/forever": "{\"n\":%d}\n",
		}[r.URL.Path]
		switch r.URL.Path {
		case "/denied":
			http.Error(w, "nope", http.StatusForbidden)
			return
		case "/bad":
			fmt.Fprint(w, "{\"n\":1}\nnot json\n")
			return
		case "/forever":
			for i := 1; ; i++ {
				if _, err := fmt.Fprintf(w, frame, i); err != nil {
					break
				}
				fl.Flush()
				select {
				case <-r.Context().Done():
					st.gone <- struct{}{}
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			return
		}
		var n int
		fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n)
		if r.URL.Path == "/sse" {
			fmt.Fprint(w, ": heartbeat\n\n")
		}
		for i := 1; i <= n; i++ {
			fmt.Fprintf(w, frame, i)
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func newStreamGateway(t *testing.T, opts ...Option) (*Gateway, *tickUpstreamState) {
	t.Helper()
	up, st := tickUpstream(t)
	opts = append([]Option{WithoutMetrics(), WithoutBackpressure(), WithoutSubscriptionAuth(), WithAdminToken([]byte("test"))}, opts...)
	gw := New(opts...)
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(tickStreamSpec), To(up.URL), As("ticks")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	return gw, st
}

func streamWS(t *testing.T, ctx context.Context, gw *Gateway) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	conn := dialWS(t, ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/graphql")
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func subscribeWS(t *testing.T, ctx context.Context, conn *websocket.Conn, id, query string) {
	t.Helper()
	p, _ := json.Marshal(subscribePayload{Query: query})
	if err := writeWS(conn, ctx, wsMessage{ID: id, Type: msgSubscribe, Payload: p}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAPIStream_GraphQLSubscription(t *testing.T) {
	gw, st := newStreamGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := streamWS(t, ctx, gw)
	for i, op := range []string{"sse", "jsonl", "seq"} {
		id := fmt.Sprint(i)
		subscribeWS(t, ctx, conn, id, fmt.Sprintf(`subscription { ticks_%s(n: 3) { n } }`, op))
		var got []int
		for {
			m, err := readWS(conn, ctx)
			if err != nil {
				t.Fatalf("%s: read: %v", op, err)
			}
			if m.Type == msgComplete {
				break
			}
			if m.Type != msgNext || m.ID != id {
				t.Fatalf("%s: frame %s %s", op, m.Type, m.Payload)
			}
			var p struct {
				Data map[string]struct{ N int } `json:"data"`
			}
			if err := json.Unmarshal(m.Payload, &p); err != nil {
				t.Fatal(err)
			}
			got = append(got, p.Data["ticks_"+op].N)
		}
		if fmt.Sprint(got) != "[1 2 3]" {
			t.Fatalf("%s: ticks = %v, want [1 2 3]", op, got)
		}
		want := map[string]string{"sse": "text/event-stream", "jsonl": "application/jsonl", "seq": "application/json-seq"}[op]
		if a := <-st.accept; a != want {
			t.Errorf("%s: upstream Accept = %q, want %q", op, a, want)
		}
	}
}

func TestOpenAPIStream_ClientCompleteClosesUpstream(t *testing.T) {
	gw, st := newStreamGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := streamWS(t, ctx, gw)
	subscribeWS(t, ctx, conn, "1", `subscription { ticks_forever { n } }`)
	for i := 0; i < 2; i++ {
		if m, err := readWS(conn, ctx); err != nil || m.Type != msgNext {
			t.Fatalf("frame %+v %v", m, err)
		}
	}
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: msgComplete}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.gone:
	case <-ctx.Done():
		t.Fatal("upstream request still open after client complete")
	}
}

func TestOpenAPIStream_Errors(t *testing.T) {
	gw, _ := newStreamGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := streamWS(t, ctx, gw)

	// Upstream status before the stream: error frame with the code.
	subscribeWS(t, ctx, conn, "1", `subscription { ticks_denied { n } }`)
	m, err := readWS(conn, ctx)
	if err != nil || m.Type != msgError {
		t.Fatalf("want error frame, got %+v %v", m, err)
	}
	if p := string(m.Payload); !strings.Contains(p, "403") || !strings.Contains(p, `"code":"`+CodePermissionDenied.String()+`"`) {
		t.Fatalf("error payload %s", p)
	}

	// Bad item mid-stream: one tick, then an error, then the end.
	subscribeWS(t, ctx, conn, "2", `subscription { ticks_bad { n } }`)
	if m, err := readWS(conn, ctx); err != nil || m.Type != msgNext {
		t.Fatalf("want next, got %+v %v", m, err)
	}
	m, err = readWS(conn, ctx)
	if err != nil || m.Type != msgError || !strings.Contains(string(m.Payload), "decode") {
		t.Fatalf("want decode error frame, got %+v %v", m, err)
	}
}

func TestOpenAPIStream_StreamCap(t *testing.T) {
	gw, _ := newStreamGateway(t, WithBackpressure(BackpressureOptions{MaxStreams: 1}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := streamWS(t, ctx, gw)
	subscribeWS(t, ctx, conn, "1", `subscription { ticks_forever { n } }`)
	if m, err := readWS(conn, ctx); err != nil || m.Type != msgNext {
		t.Fatalf("frame %+v %v", m, err)
	}
	subscribeWS(t, ctx, conn, "2", `subscription { ticks_forever { n } }`)
	for {
		m, err := readWS(conn, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != "2" {
			continue
		}
		if m.Type != msgError || !strings.Contains(string(m.Payload), "stream cap") {
			t.Fatalf("want stream cap error, got %+v", m)
		}
		break
	}

	// Completing the first frees its slot.
	if err := writeWS(conn, ctx, wsMessage{ID: "1", Type: msgComplete}); err != nil {
		t.Fatal(err)
	}
	gw.mu.Lock()
	src := gw.slots[poolKey{namespace: "ticks", version: "v1"}].openapi
	gw.mu.Unlock()
	for src.streamInflight.Load() != 0 {
		if ctx.Err() != nil {
			t.Fatal("stream slot never released")
		}
		time.Sleep(5 * time.Millisecond)
	}
	subscribeWS(t, ctx, conn, "3", `subscription { ticks_forever { n } }`)
	for {
		m, err := readWS(conn, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != "3" {
			continue
		}
		if m.Type != msgNext {
			t.Fatalf("after release: %+v", m)
		}
		break
	}
}

func TestOpenAPIStream_RESTIngressRelays(t *testing.T) {
	gw, _ := newStreamGateway(t)
	srv := httptest.NewServer(gw.IngressHandler())
	t.Cleanup(srv.Close)
	for _, c := range []struct{ path, ctype, body string }{
		{"/jsonl?n=2", "application/jsonl", "{\"n\":1}\n{\"n\":2}\n"},
		{"/seq?n=2", "application/json-seq", "\x1e{\"n\":1}\n\x1e{\"n\":2}\n"},
		{"/sse?n=2", "text/event-stream", "data: {\"n\":1}\n\ndata: {\"n\":2}\n\n"},
	} {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != c.ctype || string(b) != c.body {
			t.Errorf("%s: %d %q %q, want %q %q", c.path, resp.StatusCode, resp.Header.Get("Content-Type"), b, c.ctype, c.body)
		}
	}
	resp, err := http.Get(srv.URL + "/denied")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("/denied: status %d, want 403", resp.StatusCode)
	}

	// The relay holds the upstream open while the client reads.
	resp, err = http.Get(srv.URL + "/forever")
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if err != nil || line != "{\"n\":1}\n" {
		t.Errorf("/forever first line %q %v", line, err)
	}
}

func TestOpenAPIStream_GRPCServerStream(t *testing.T) {
	gw, _ := newStreamGateway(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	inDesc, outDesc := (&crossFormatFixture{gw: gw}).synthMethodDescriptors(t, "ticks", "v1", "jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/ticks.v1.Service/jsonl")
	if err != nil {
		t.Fatal(err)
	}
	in := dynamicpb.NewMessage(inDesc)
	in.Set(inDesc.Fields().ByName("n"), protoreflect.ValueOfInt32(3))
	if err := stream.SendMsg(in); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	var got []int64
	for {
		out := dynamicpb.NewMessage(outDesc)
		if err := stream.RecvMsg(out); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		got = append(got, out.Get(outDesc.Fields().ByName("n")).Int())
	}
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("ticks = %v, want [1 2 3]", got)
	}
}
