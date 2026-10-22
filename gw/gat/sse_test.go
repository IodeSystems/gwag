package gat_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/danielgtaylor/huma/v2/sse"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

type sseInput struct {
	Session string `query:"session" required:"true"`
	After   int64  `query:"after"`
}

type SessionEvent struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`
	By   string `json:"by"`
}

type SessionSnapshot struct {
	Seq   int64  `json:"seq"`
	State string `json:"state"`
}

// observed is one delivered event in a surface-neutral form: the SSE
// event name and the payload as generic JSON.
type observed struct {
	Event string
	Data  map[string]any
}

// newSSEGat registers one RegisterSSE op with two event types behind
// requireUser, plus a unary op, on REST + GraphQL + gRPC. The handler
// is finite: events seq (after, 3], a heartbeat comment, a snapshot.
func newSSEGat(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)

	gat.Register(api, g, huma.Operation{
		OperationID: "getProject",
		Method:      http.MethodGet,
		Path:        "/projects/{id}",
	}, func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
		return &getProjectOutput{Body: project{ID: in.ID}}, nil
	})

	gat.RegisterSSE(api, g, huma.Operation{
		OperationID: "sessionEvents",
		Method:      http.MethodGet,
		Path:        "/sessions/events",
		Summary:     "Events of a session after a seq.",
		Middlewares: huma.Middlewares{requireUser(api)},
	}, map[string]any{
		"event":    SessionEvent{},
		"snapshot": SessionSnapshot{},
	}, func(ctx context.Context, in *sseInput, send sse.Sender) {
		user, _ := ctx.Value(principalKey{}).(string)
		if user == "" {
			t.Errorf("handler reached without a principal")
		}
		for seq := in.After + 1; seq <= 3; seq++ {
			if send.Data(SessionEvent{Seq: seq, Kind: "replay", By: user}) != nil {
				return
			}
		}
		_ = send.Comment("heartbeat")
		_ = send(sse.Message{ID: 9, Retry: 1000, Data: &SessionSnapshot{Seq: 3, State: "open"}})
	})

	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatalf("RegisterGRPC: %v", err)
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// The handler's events, in order, as every surface must deliver them.
var wantSSEEvents = []observed{
	{"event", map[string]any{"seq": "2", "kind": "replay", "by": "alice"}},
	{"event", map[string]any{"seq": "3", "kind": "replay", "by": "alice"}},
	{"snapshot", map[string]any{"seq": "3", "state": "open"}},
}

// normSeq makes seq comparable across surfaces: REST JSON carries a
// number, GraphQL's Long and protojson's int64 carry a string.
func normSeq(m map[string]any) map[string]any {
	if v, ok := m["seq"]; ok {
		m["seq"] = fmt.Sprint(v)
	}
	return m
}

func readREST(t *testing.T, ctx context.Context, url, user string) (int, []observed) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var out []observed
	var event string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("SSE data: %v", err)
			}
			if event == "" {
				event = "message"
			}
			out = append(out, observed{event, normSeq(m)})
			event = ""
		}
	}
	return resp.StatusCode, out
}

const sseSubscription = `subscription {
  Demo_sessionEvents(session: "s1", after: 1) {
    __typename
    ... on Demo_SessionEvent { seq kind by }
    ... on Demo_SessionSnapshot { seq state }
  }
}`

// graphQLEventName maps a GraphQL __typename back to the SSE event
// name declared for that type.
var graphQLEventName = map[string]string{"Demo_SessionEvent": "event", "Demo_SessionSnapshot": "snapshot"}

func readGraphQL(t *testing.T, ctx context.Context, url, user string) ([]observed, wsMsg) {
	t.Helper()
	conn := dialGraphQLWS(t, ctx, url, user)
	wsSend(t, ctx, conn, subscribeMsg("1", sseSubscription))
	var out []observed
	for {
		m := wsRecv(t, ctx, conn)
		switch m.Type {
		case "next":
			var res map[string]any
			_ = json.Unmarshal(m.Payload, &res)
			ev, _ := digPath(res, "data", "Demo_sessionEvents").(map[string]any)
			if ev == nil {
				t.Fatalf("next without event: %s", m.Payload)
			}
			name := fmt.Sprint(ev["__typename"])
			delete(ev, "__typename")
			out = append(out, observed{graphQLEventName[name], normSeq(ev)})
		case "complete", "error":
			return out, m
		default:
			t.Fatalf("unexpected frame %+v", m)
		}
	}
}

func sseMethod(t *testing.T, srvURL string) protoreflect.MethodDescriptor {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srvURL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	return findMethod(t, files, "sessionEvents")
}

// protoEventName maps the oneof member set on a stream message back to
// the SSE event name.
var protoEventName = map[string]string{"session_event": "event", "session_snapshot": "snapshot"}

func readGRPC(t *testing.T, ctx context.Context, srv *httptest.Server, md protoreflect.MethodDescriptor, user string) ([]observed, error) {
	t.Helper()
	procedure := "/" + string(md.Parent().(protoreflect.ServiceDescriptor).FullName()) + "/" + string(md.Name())
	client := connect.NewClient[dynamicpb.Message, dynamicpb.Message](
		srv.Client(), srv.URL+"/api/grpc"+procedure,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(spec connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(spec.Schema.(protoreflect.MethodDescriptor).Output())
			return nil
		}),
	)
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("session"), protoreflect.ValueOfString("s1"))
	in.Set(md.Input().Fields().ByName("after"), protoreflect.ValueOfInt64(1))
	req := connect.NewRequest(in)
	if user != "" {
		req.Header().Set("X-User", user)
	}
	stream, err := client.CallServerStream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var out []observed
	for stream.Receive() {
		msg := stream.Msg()
		oneof := msg.Descriptor().Oneofs().ByName("value")
		if oneof == nil {
			t.Fatalf("stream message %s has no oneof value", msg.Descriptor().FullName())
		}
		set := msg.WhichOneof(oneof)
		if set == nil {
			t.Fatalf("stream message with no oneof member set: %v", msg)
		}
		b, _ := protojson.Marshal(msg.Get(set).Message().Interface())
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, observed{protoEventName[string(set.Name())], normSeq(m)})
	}
	return out, stream.Err()
}

// Pins: one RegisterSSE handler is served on REST (SSE), GraphQL
// (graphql-transport-ws) and gRPC (server stream) with the same
// events in the same order, behind the same middleware.
func TestRegisterSSE_SurfacesEquivalent(t *testing.T) {
	srv := newSSEGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, rest := readREST(t, ctx, srv.URL+"/sessions/events?session=s1&after=1", "alice")
	if status != http.StatusOK || !reflect.DeepEqual(rest, wantSSEEvents) {
		t.Errorf("REST: status %d events %v\nwant %v", status, rest, wantSSEEvents)
	}

	gql, end := readGraphQL(t, ctx, srv.URL+"/api/graphql", "alice")
	if end.Type != "complete" || !reflect.DeepEqual(gql, wantSSEEvents) {
		t.Errorf("GraphQL: end %s %s events %v\nwant %v", end.Type, end.Payload, gql, wantSSEEvents)
	}

	grpc, err := readGRPC(t, ctx, srv, sseMethod(t, srv.URL), "alice")
	if err != nil || !reflect.DeepEqual(grpc, wantSSEEvents) {
		t.Errorf("gRPC: err %v events %v\nwant %v", err, grpc, wantSSEEvents)
	}
}

func TestRegisterSSE_MiddlewareRejectsEverywhere(t *testing.T) {
	srv := newSSEGat(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if status, _ := readREST(t, ctx, srv.URL+"/sessions/events?session=s1", ""); status != http.StatusUnauthorized {
		t.Errorf("REST status = %d, want 401", status)
	}
	if evs, end := readGraphQL(t, ctx, srv.URL+"/api/graphql", ""); end.Type != "error" || len(evs) != 0 ||
		!strings.Contains(string(end.Payload), `"status":401`) {
		t.Errorf("GraphQL: events %v end %s %s, want a 401 error frame", evs, end.Type, end.Payload)
	}
	if evs, err := readGRPC(t, ctx, srv, sseMethod(t, srv.URL), ""); connect.CodeOf(err) != connect.CodeUnauthenticated || len(evs) != 0 {
		t.Errorf("gRPC: events %v err %v, want Unauthenticated", evs, err)
	}
}

// Pins the generated shapes: a GraphQL union subscription field and a
// server-streaming proto method returning a oneof message, both over
// the same argument list.
func TestRegisterSSE_Shapes(t *testing.T) {
	srv := newSSEGat(t)
	sdl := string(mustGET(t, srv.URL+"/api/schema/graphql"))
	for _, want := range []string{
		"union Demo_SessionEventsEvent = Demo_SessionEvent | Demo_SessionSnapshot",
		"Demo_sessionEvents(",
		"session: String!",
		"after: Long",
		`): Demo_SessionEventsEvent! @derived(operationId: "sessionEvents")`,
	} {
		if !strings.Contains(sdl, want) {
			t.Errorf("SDL missing %q:\n%s", want, sdl)
		}
	}

	md := sseMethod(t, srv.URL)
	if !md.IsStreamingServer() || md.IsStreamingClient() {
		t.Errorf("proto method: server-streaming %v client-streaming %v", md.IsStreamingServer(), md.IsStreamingClient())
	}
	if got := md.Output().Name(); got != "SessionEventsEvent" {
		t.Errorf("proto output = %s, want SessionEventsEvent", got)
	}
	for _, f := range []string{"session", "after"} {
		if md.Input().Fields().ByName(protoreflect.Name(f)) == nil {
			t.Errorf("proto input missing %s", f)
		}
	}

	spec := string(mustGET(t, srv.URL+"/openapi.json"))
	if !strings.Contains(spec, "text/event-stream") {
		t.Errorf("REST spec lacks the text/event-stream response")
	}
}

// One event type: the GraphQL field and the proto stream message are
// that type itself, no union. The handler streams until its ctx ends,
// which a GraphQL complete must cause.
func TestRegisterSSE_SingleTypeAndCancel(t *testing.T) {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t)
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			return &getProjectOutput{Body: project{ID: in.ID}}, nil
		})
	stopped := make(chan struct{})
	gat.RegisterSSE(api, g, huma.Operation{OperationID: "ticks", Method: http.MethodGet, Path: "/ticks"},
		map[string]any{"message": SessionEvent{}},
		func(ctx context.Context, in *struct{}, send sse.Sender) {
			defer close(stopped)
			for seq := int64(1); ; seq++ {
				if send.Data(&SessionEvent{Seq: seq, Kind: "tick"}) != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatalf("RegisterHuma: %v", err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatalf("RegisterGRPC: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if sdl := string(mustGET(t, srv.URL+"/api/schema/graphql")); !strings.Contains(sdl, "Demo_ticks: Demo_SessionEvent!") {
		t.Errorf("SDL lacks Demo_ticks: Demo_SessionEvent!:\n%s", sdl)
	}
	fds := &descriptorpb.FileDescriptorSet{}
	_ = proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds)
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	if md := findMethod(t, files, "ticks"); !md.IsStreamingServer() || md.Output().Name() != "SessionEvent" {
		t.Errorf("proto ticks: streaming %v output %s, want server stream of SessionEvent", md.IsStreamingServer(), md.Output().Name())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialGraphQLWS(t, ctx, srv.URL+"/api/graphql", "")
	wsSend(t, ctx, conn, subscribeMsg("1", `subscription { Demo_ticks { seq kind } }`))
	if m := wsRecv(t, ctx, conn); m.Type != "next" || !strings.Contains(string(m.Payload), `"kind":"tick"`) {
		t.Fatalf("want a tick, got %+v %s", m, m.Payload)
	}
	wsSend(t, ctx, conn, wsMsg{ID: "1", Type: "complete"})
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("handler ctx not cancelled after complete")
	}
}
