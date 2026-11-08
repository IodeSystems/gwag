package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mark3labs/mcp-go/server/servertest"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	psv1 "github.com/iodesystems/gwag/v2/gw/proto/ps/v1"
)

// replayUpstreams are the services both gateways call. They are
// shared, so their counters are the total number of upstream calls
// across blue and green.
type replayUpstreams struct {
	greeter    *fakeGreeterServer
	grpcAddr   string
	openapi    *httptest.Server
	gets       atomic.Int32
	posts      atomic.Int32
	lastHeader atomic.Pointer[http.Header]
	remote     *remoteFixture
	remoteN    atomic.Int32
	mcp        *httptest.Server
	mcpN       atomic.Int32
	inprocN    atomic.Int32
}

func newReplayUpstreams(t *testing.T) *replayUpstreams {
	t.Helper()
	u := &replayUpstreams{greeter: &fakeGreeterServer{}}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(srv, u.greeter)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	u.grpcAddr = lis.Addr().String()

	u.openapi = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		u.lastHeader.Store(&h)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			u.posts.Add(1)
			_, _ = w.Write([]byte(`{"id":"new-1","name":"widget"}`))
		default:
			u.gets.Add(1)
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = w.Write([]byte(`{"id":"` + id + `","name":"thing"}`))
		}
	}))
	t.Cleanup(u.openapi.Close)

	u.remote = newRemoteFixture(t)
	u.remote.queryHandler = func(string, map[string]any) any {
		u.remoteN.Add(1)
		return map[string]any{"users": []map[string]any{{"id": "1", "name": "alice", "role": "ADMIN"}}}
	}

	ms := mcpserver.NewMCPServer("replay-mcp", "1.0.0", mcpserver.WithToolCapabilities(false))
	ms.AddTool(
		mcp.NewTool("echo", mcp.WithString("message", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			u.mcpN.Add(1)
			return mcp.NewToolResultText("echo: " + req.GetArguments()["message"].(string)), nil
		},
	)
	u.mcp = servertest.NewTestStreamableHTTPServer(ms)
	t.Cleanup(u.mcp.Close)
	return u
}

// register adds every upstream to g. opts apply to the OpenAPI and
// GraphQL sources; proto takes protoOpts.
func (u *replayUpstreams) register(t *testing.T, g *Gateway, spec string, protoOpts, httpOpts []ServiceOption) {
	t.Helper()
	if err := g.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
		append([]ServiceOption{To(u.grpcAddr), As("greeter")}, protoOpts...)...); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	if spec == "" {
		spec = minimalOpenAPISpec
	}
	if err := g.AddOpenAPIBytes([]byte(spec), append([]ServiceOption{To(u.openapi.URL), As("test")}, httpOpts...)...); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	if err := g.AddGraphQL(u.remote.server.URL, append([]ServiceOption{As("pets")}, httpOpts...)...); err != nil {
		t.Fatalf("AddGraphQL: %v", err)
	}
	if err := g.AddMCP(MCPHTTP, u.mcp.URL, As("tools")); err != nil {
		t.Fatalf("AddMCP: %v", err)
	}
	handlers := map[string]internalProtoHandler{
		"Pub": func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			u.inprocN.Add(1)
			return &psv1.PubResponse{}, nil
		},
	}
	g.mu.Lock()
	err := g.addInternalProtoSlotLocked("intpb", "v1", psv1.File_gw_proto_ps_v1_ps_proto, nil, handlers, nil)
	g.mu.Unlock()
	if err != nil {
		t.Fatalf("addInternalProtoSlotLocked: %v", err)
	}
}

type replayCaptured struct {
	ex  ReplayExchange
	res ReplayResult
}

// replayRig is blue and green over shared upstreams. Blue posts its
// recordings to a capture server that runs green.ReplayVerify, so a
// test sees both the recording and the result.
type replayRig struct {
	u        *replayUpstreams
	blue     *Gateway
	green    *Gateway
	blueSrv  *httptest.Server
	captured chan replayCaptured
}

type replaySide struct {
	spec      string
	protoOpts []ServiceOption
	httpOpts  []ServiceOption
	setup     func(*Gateway)
}

func newReplayRig(t *testing.T, blueSide, greenSide replaySide, blueOpts ReplayVerifyOptions) *replayRig {
	t.Helper()
	rig := &replayRig{u: newReplayUpstreams(t), captured: make(chan replayCaptured, 32)}

	rig.green = New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("green-admin")))
	t.Cleanup(rig.green.Close)
	if greenSide.setup != nil {
		greenSide.setup(rig.green)
	}
	rig.u.register(t, rig.green, greenSide.spec, greenSide.protoOpts, greenSide.httpOpts)

	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ex ReplayExchange
		if err := json.NewDecoder(r.Body).Decode(&ex); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		res := rig.green.ReplayVerify(r.Context(), &ex)
		rig.captured <- replayCaptured{ex: ex, res: res}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(capture.Close)

	if blueOpts.Target == "" {
		blueOpts.Target = capture.URL
	}
	rig.blue = New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("blue-admin")), WithReplayVerify(blueOpts))
	t.Cleanup(rig.blue.Close)
	if blueSide.setup != nil {
		blueSide.setup(rig.blue)
	}
	rig.u.register(t, rig.blue, blueSide.spec, blueSide.protoOpts, blueSide.httpOpts)
	rig.blueSrv = httptest.NewServer(rig.blue.Handler())
	t.Cleanup(rig.blueSrv.Close)
	return rig
}

func replayPost(t *testing.T, url, query string, header http.Header) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query})
	req, _ := http.NewRequest(http.MethodPost, url+"/graphql", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), `"errors"`) {
		t.Fatalf("graphql errors: %s", b)
	}
	return string(b)
}

func (rig *replayRig) next(t *testing.T) replayCaptured {
	t.Helper()
	select {
	case c := <-rig.captured:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no recording reached green")
	}
	return replayCaptured{}
}

// upstreamCalls is the total of every upstream counter.
func (u *replayUpstreams) upstreamCalls() int32 {
	return u.greeter.helloCalls.Load() + u.gets.Load() + u.posts.Load() + u.remoteN.Load() + u.mcpN.Load() + u.inprocN.Load()
}

// hasDiff reports a diff at path or below it.
func hasDiff(res ReplayResult, path string) bool {
	for _, d := range res.Diffs {
		if d.Path == path || strings.HasPrefix(d.Path, path+".") || strings.HasPrefix(d.Path, path+"[") {
			return true
		}
	}
	return false
}

var replayKindCases = []struct {
	name  string
	kind  string
	query string
}{
	{"grpc", replayKindGRPC, `{ greeter { hello(name:"world") { greeting } } }`},
	{"openapi", replayKindOpenAPI, `{ test { getThing(id:"42") { id name } } }`},
	{"openapi_mutation", replayKindOpenAPI, `mutation { test { createThing(body:{name:"widget"}) { id name } } }`},
	{"graphql", replayKindGraphQL, `{ pets { users { id name role } } }`},
	{"mcp_mutation", replayKindMCP, `mutation { tools { echo(message:"hi") } }`},
	{"inproc", replayKindInproc, `{ intpb { pub(channel:"events.a", payload:"p") { _void } } }`},
}

// TestReplayVerify_PerKind: for each upstream kind, the recorded
// request matches on green with one upstream call in total (green
// called nothing, mutations included); the same recording without its
// legs is unverifiable and still calls nothing; with an extra blue leg
// it is a diff (not_called).
func TestReplayVerify_PerKind(t *testing.T) {
	rig := newReplayRig(t, replaySide{}, replaySide{}, ReplayVerifyOptions{})
	for _, tc := range replayKindCases {
		t.Run(tc.name, func(t *testing.T) {
			before := rig.u.upstreamCalls()
			replayPost(t, rig.blueSrv.URL, tc.query, nil)
			c := rig.next(t)
			if c.res.Outcome != ReplayOutcomeMatch {
				t.Fatalf("outcome=%s err=%q diffs=%+v\nblue=%s\ngreen=%s", c.res.Outcome, c.res.Error, c.res.Diffs, c.ex.Response.Body, c.res.Response.Body)
			}
			if len(c.ex.Legs) != 1 || c.ex.Legs[0].Kind != tc.kind {
				t.Fatalf("legs=%+v, want one %s leg", c.ex.Legs, tc.kind)
			}
			if len(c.res.Legs) != 1 || c.res.Legs[0].Outcome != replayLegMatch {
				t.Fatalf("result legs=%+v", c.res.Legs)
			}
			if got := rig.u.upstreamCalls() - before; got != 1 {
				t.Fatalf("upstream calls=%d, want 1 (green must not call upstream)", got)
			}

			noLegs := c.ex
			noLegs.Legs = nil
			res := rig.green.ReplayVerify(context.Background(), &noLegs)
			if res.Outcome != ReplayOutcomeUnverifiable {
				t.Fatalf("without legs: outcome=%s, want unverifiable; diffs=%+v", res.Outcome, res.Diffs)
			}
			if len(res.Legs) != 1 || res.Legs[0].Outcome != replayLegUnverifiable || res.Legs[0].Request == nil {
				t.Fatalf("without legs: legs=%+v", res.Legs)
			}
			if got := rig.u.upstreamCalls() - before; got != 1 {
				t.Fatalf("after unverifiable replay: upstream calls=%d, want 1", got)
			}

			extra := c.ex
			extra.Legs = append(append([]ReplayLeg{}, c.ex.Legs...), c.ex.Legs[0])
			res = rig.green.ReplayVerify(context.Background(), &extra)
			if res.Outcome != ReplayOutcomeDiff || len(res.Legs) != 2 || res.Legs[1].Outcome != replayLegNotCalled {
				t.Fatalf("extra blue leg: outcome=%s legs=%+v", res.Outcome, res.Legs)
			}
		})
	}
}

// TestReplayVerify_TransformDiff: an injected header that differs
// between the clusters is a leg diff on gRPC metadata and on the
// OpenAPI request; a forwarded header green adds is a diff on the
// downstream GraphQL request. Client responses still match.
func TestReplayVerify_TransformDiff(t *testing.T) {
	inject := func(v string) func(*Gateway) {
		return func(g *Gateway) {
			g.Use(InjectHeader("x-tenant", func(context.Context, *string) (string, error) { return v, nil }))
		}
	}
	rig := newReplayRig(t,
		replaySide{setup: inject("blue")},
		replaySide{setup: inject("green"), httpOpts: []ServiceOption{ForwardHeaders("Authorization", "X-Caller")}},
		ReplayVerifyOptions{})
	for _, tc := range []struct {
		name, query, path string
	}{
		{"grpc", `{ greeter { hello(name:"world") { greeting } } }`, "legs[0].request.header.x-tenant"},
		{"openapi", `{ test { getThing(id:"42") { id } } }`, "legs[0].request.header.x-tenant"},
		{"graphql", `{ pets { users { id } } }`, "legs[0].request.header.x-caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayPost(t, rig.blueSrv.URL, tc.query, http.Header{"X-Caller": {"svc-a"}})
			c := rig.next(t)
			if c.res.Outcome != ReplayOutcomeDiff || !hasDiff(c.res, tc.path) {
				t.Fatalf("outcome=%s diffs=%+v, want diff at %s", c.res.Outcome, c.res.Diffs, tc.path)
			}
			for _, d := range c.res.Diffs {
				if strings.HasPrefix(d.Path, "response.") {
					t.Errorf("client response differs: %+v", d)
				}
				if strings.Contains(d.Path, "traceparent") {
					t.Errorf("trace header compared: %+v", d)
				}
			}
		})
	}
}

// renamedPathSpec is minimalOpenAPISpec with getThing moved to
// /v2/things/{id}: a routing change for the same operation.
var renamedPathSpec = strings.Replace(minimalOpenAPISpec, `"/things/{id}"`, `"/v2/things/{id}"`, 1)

// TestReplayVerify_RoutingDiff: green sends the same operation to
// another version (gRPC) or another path (OpenAPI).
func TestReplayVerify_RoutingDiff(t *testing.T) {
	rig := newReplayRig(t,
		replaySide{},
		replaySide{spec: renamedPathSpec, protoOpts: []ServiceOption{Version("v2")}},
		ReplayVerifyOptions{})
	for _, tc := range []struct {
		name, query, path string
	}{
		{"grpc", `{ greeter { hello(name:"world") { greeting } } }`, "legs[0].request.version"},
		{"openapi", `{ test { getThing(id:"42") { id } } }`, "legs[0].request.path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayPost(t, rig.blueSrv.URL, tc.query, nil)
			c := rig.next(t)
			if c.res.Outcome != ReplayOutcomeDiff || !hasDiff(c.res, tc.path) {
				t.Fatalf("outcome=%s diffs=%+v, want diff at %s", c.res.Outcome, c.res.Diffs, tc.path)
			}
		})
	}
}

// TestReplayVerify_ResponseDiff: green's own transform changes the
// client response (a Runtime middleware rewrites the greeting).
func TestReplayVerify_ResponseDiff(t *testing.T) {
	rewrite := Transform{Runtime: func(next Handler) Handler {
		return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			resp, err := next(ctx, req)
			if err != nil {
				return nil, err
			}
			m := resp.ProtoReflect()
			if fd := m.Descriptor().Fields().ByName("greeting"); fd != nil {
				m.Set(fd, protoreflect.ValueOfString("changed"))
			}
			return resp, nil
		}
	}}
	rig := newReplayRig(t, replaySide{}, replaySide{setup: func(g *Gateway) { g.Use(rewrite) }}, ReplayVerifyOptions{})
	replayPost(t, rig.blueSrv.URL, `{ greeter { hello(name:"world") { greeting } } }`, nil)
	c := rig.next(t)
	if c.res.Outcome != ReplayOutcomeDiff || !hasDiff(c.res, "response.body.data.greeter.hello.greeting") {
		t.Fatalf("outcome=%s diffs=%+v", c.res.Outcome, c.res.Diffs)
	}
	if len(c.res.Legs) != 1 || c.res.Legs[0].Outcome != replayLegMatch {
		t.Fatalf("legs=%+v, want the upstream request to match", c.res.Legs)
	}
}

// TestReplayVerify_Sample covers the match rules.
func TestReplayVerify_Sample(t *testing.T) {
	rr := newReplayRecorder(nil, ReplayVerifyOptions{Target: "x", Match: []ReplayMatch{
		{OperationName: "GetThing"},
		{Header: "X-Canary", HeaderValue: "1"},
		{Header: "X-Never", SampleRate: 0.0000001},
	}})
	for _, tc := range []struct {
		op   string
		h    http.Header
		want bool
	}{
		{"GetThing", nil, true},
		{"Other", nil, false},
		{"Other", http.Header{"X-Canary": {"1"}}, true},
		{"Other", http.Header{"X-Canary": {"2"}}, false},
	} {
		if got := rr.sample(tc.op, tc.h); got != tc.want {
			t.Errorf("sample(%q, %v)=%v, want %v", tc.op, tc.h, got, tc.want)
		}
	}
	hits := 0
	for i := 0; i < 1000; i++ {
		if rr.sample("Other", http.Header{"X-Never": {"x"}}) {
			hits++
		}
	}
	if hits > 1 {
		t.Errorf("rate 1e-7 sampled %d of 1000", hits)
	}
	if name, ok := replayOperationName([]byte(`{"query":"query GetThing { a }"}`), true); !ok || name != "GetThing" {
		t.Errorf("operation name=%q ok=%v", name, ok)
	}
	if _, ok := replayOperationName([]byte(`[{"query":"{ a }"}]`), true); ok {
		t.Error("a batch must not be recorded")
	}
}

// TestReplayVerify_QueueFullDrops: with green stuck, recordings past
// the queue are dropped and counted, and client requests still finish.
func TestReplayVerify_QueueFullDrops(t *testing.T) {
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(stuck.Close)
	t.Cleanup(func() { close(release) })
	rig := newReplayRig(t, replaySide{}, replaySide{}, ReplayVerifyOptions{Target: stuck.URL, QueueSize: 1, Workers: 1})
	start := time.Now()
	for i := 0; i < 5; i++ {
		replayPost(t, rig.blueSrv.URL, `{ greeter { hello(name:"world") { greeting } } }`, nil)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("client requests took %s with green stuck", d)
	}
	if n := rig.blue.replayRec.countOf(ReplayOutcomeDropped); n < 3 {
		t.Fatalf("dropped=%d, want >= 3 (one in flight, one queued)", n)
	}
}

// TestReplayVerify_TwoGateways runs the real path: blue posts to
// green's admin endpoint behind AdminMiddleware with green's admin
// token. One upstream call in total, the match is counted on blue's
// replay_verify_total, a diff shows on blue's GET /admin/replay/diffs.
func TestReplayVerify_TwoGateways(t *testing.T) {
	u := newReplayUpstreams(t)
	greenTok := []byte("green-admin-token")
	green := New(WithoutBackpressure(), WithAdminToken(greenTok), WithReplayVerifyEndpoint())
	t.Cleanup(green.Close)
	u.register(t, green, "", nil, nil)
	greenAdmin, _, err := green.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	greenMux := http.NewServeMux()
	greenMux.Handle("/api/admin/", http.StripPrefix("/api", green.AdminMiddleware(greenAdmin)))
	greenSrv := httptest.NewServer(greenMux)
	t.Cleanup(greenSrv.Close)
	target := greenSrv.URL + "/api/admin/replay/verify"

	// The endpoint needs the admin role: no token and the dev token
	// are refused.
	for name, tok := range map[string]string{"anonymous": "", "dev": green.DevTokenHex()} {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 401/403", name, resp.StatusCode)
		}
	}

	blueTok := []byte("blue-admin-token")
	blue := New(WithoutBackpressure(), WithAdminToken(blueTok), WithReplayVerify(ReplayVerifyOptions{
		Target: target,
		Token:  hex.EncodeToString(greenTok),
		Match:  []ReplayMatch{{OperationName: "Hello"}, {OperationName: "Thing"}},
	}))
	t.Cleanup(blue.Close)
	u.register(t, blue, "", nil, nil)
	blueSrv := httptest.NewServer(blue.Handler())
	t.Cleanup(blueSrv.Close)

	waitCount := func(outcome string, want int64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for blue.replayRec.countOf(outcome) < want {
			if time.Now().After(deadline) {
				t.Fatalf("%s=%d, want %d; recent=%+v", outcome, blue.replayRec.countOf(outcome), want, blue.replayRec.recentResults())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Not matched by any rule: not recorded.
	replayPost(t, blueSrv.URL, `query Other { greeter { hello(name:"x") { greeting } } }`, nil)
	replayPost(t, blueSrv.URL, `query Hello { greeter { hello(name:"world") { greeting } } }`, nil)
	waitCount(ReplayOutcomeMatch, 1)
	if got := u.greeter.helloCalls.Load(); got != 2 {
		t.Fatalf("greeter calls=%d, want 2 (one per client request, none from green)", got)
	}

	replayPost(t, blueSrv.URL, `query Thing { test { getThing(id:"7") { id name } } }`, nil)
	waitCount(ReplayOutcomeMatch, 2)
	if got := u.gets.Load(); got != 1 {
		t.Fatalf("openapi calls=%d, want 1", got)
	}

	mr := httptest.NewRecorder()
	blue.MetricsHandler().ServeHTTP(mr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(mr.Body.String(), `replay_verify_total{outcome="match"} 2`) {
		t.Fatalf("metrics missing replay_verify_total match=2:\n%s", grepLines(mr.Body.String(), "replay_verify"))
	}

	// A diff lands in blue's recent list, readable by blue's admin.
	blue.replayRec.done(&ReplayExchange{ID: "x1", OperationName: "Hello"}, ReplayResult{ID: "x1", Outcome: ReplayOutcomeDiff, Diffs: []ReplayDiff{{Path: "response.body.a", Blue: "1", Green: "2"}}})
	blueAdmin, _, err := blue.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	bm := http.NewServeMux()
	bm.Handle("/api/admin/", http.StripPrefix("/api", blue.AdminMiddleware(blueAdmin)))
	get := func(tok string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/replay/diffs", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		bm.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := get(""); code == http.StatusOK {
		t.Fatal("GET /admin/replay/diffs answered without a token")
	}
	if code, _ := get(blue.DevTokenHex()); code == http.StatusOK {
		t.Fatal("GET /admin/replay/diffs answered the dev role")
	}
	code, body := get(hex.EncodeToString(blueTok))
	if code != http.StatusOK || !strings.Contains(body, `"response.body.a"`) || !strings.Contains(body, `"match":2`) {
		t.Fatalf("diffs: %d %s", code, body)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
