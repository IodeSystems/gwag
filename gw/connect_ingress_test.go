package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

// serveConnect mounts h under /rpc on an h2c server.
func serveConnect(t *testing.T, h http.Handler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/rpc/", http.StripPrefix("/rpc", h))
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL + "/rpc"
}

func h2cHTTPClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

// connectProtocols are the client settings ConnectHandler serves.
var connectProtocols = map[string]struct {
	client *http.Client
	opts   []connect.ClientOption
}{
	"connect":      {http.DefaultClient, nil},
	"connect-json": {http.DefaultClient, []connect.ClientOption{connect.WithProtoJSON()}},
	"grpc-web":     {http.DefaultClient, []connect.ClientOption{connect.WithGRPCWeb()}},
	"grpc":         {h2cHTTPClient(), []connect.ClientOption{connect.WithGRPC()}},
}

// Added with the move onto gat's gRPC ingress: the routes
// GRPCUnknownHandler serves are reachable over Connect, gRPC-Web and
// gRPC through ConnectHandler.
func TestConnectIngress_Unary(t *testing.T) {
	f := newGRPCIngressFixture(t)
	url := serveConnect(t, f.gw.ConnectHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, p := range connectProtocols {
		c := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](p.client, url+"/greeter.v1.GreeterService/Hello", p.opts...)
		resp, err := c.CallUnary(ctx, connect.NewRequest(&greeterv1.HelloRequest{Name: name}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := resp.Msg.GetGreeting(); got != "hello "+name {
			t.Errorf("%s: greeting %q", name, got)
		}
	}
	if got := f.greeter.helloCalls.Load(); got != int32(len(connectProtocols)) {
		t.Errorf("backend calls = %d, want %d", got, len(connectProtocols))
	}
	resp, err := http.Post(url+"/greeter.v1.GreeterService/Nope", "application/proto", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown procedure: %d", resp.StatusCode)
	}
}

func TestConnectIngress_ServerStreaming(t *testing.T) {
	f := newGRPCStreamingFixture(t)
	url := serveConnect(t, f.gw.ConnectHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, p := range connectProtocols {
		c := connect.NewClient[greeterv1.GreetingsFilter, greeterv1.Greeting](p.client, url+"/greeter.v1.GreeterService/Greetings", p.opts...)
		stream, err := c.CallServerStream(ctx, connect.NewRequest(&greeterv1.GreetingsFilter{Name: name}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !stream.Receive() {
			t.Fatalf("%s: no message: %v", name, stream.Err())
		}
		if got := stream.Msg().GetGreeting(); got != "hello "+name {
			t.Errorf("%s: greeting %q", name, got)
		}
		_ = stream.Close()
	}
}

// Draining refuses new gRPC calls with Unavailable on every transport.
func TestGRPCIngress_DrainingIsUnavailable(t *testing.T) {
	f := newGRPCIngressFixture(t)
	url := serveConnect(t, f.gw.ConnectHandler())
	f.gw.draining.Store(true)
	defer f.gw.draining.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.cli.Hello(ctx, &greeterv1.HelloRequest{Name: "x"}); status.Code(err) != codes.Unavailable {
		t.Errorf("grpc: %v", err)
	}
	c := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](http.DefaultClient, url+"/greeter.v1.GreeterService/Hello")
	if _, err := c.CallUnary(ctx, connect.NewRequest(&greeterv1.HelloRequest{Name: "x"})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("connect: %v", err)
	}
	if got := f.greeter.helloCalls.Load(); got != 0 {
		t.Errorf("backend calls while draining: %d", got)
	}
}

// recordingDispatcher answers a subscription with one event and records
// the args it was called with.
type recordingDispatcher struct {
	mu   sync.Mutex
	args []map[string]any
}

func (d *recordingDispatcher) Dispatch(_ context.Context, args map[string]any) (any, error) {
	d.mu.Lock()
	d.args = append(d.args, args)
	d.mu.Unlock()
	ch := make(chan any, 1)
	ch <- map[string]any{"greeting": "hi", "forName": args["name"]}
	close(ch)
	return ch, nil
}

// The subscription HMAC handles travel as gRPC metadata, or as headers
// over Connect and gRPC-Web, into the dispatcher's args.
func TestGRPCIngress_SubscriptionAuthHandles(t *testing.T) {
	file, err := compileProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"), nil)
	if err != nil {
		t.Fatal(err)
	}
	md := file.Services().Get(0).Methods().ByName("Greetings")
	disp := &recordingDispatcher{}
	g, _ := gat.New()
	if err := g.SetGRPCRoutes([]gat.GRPCRoute{{Method: md, Stream: grpcSubscription(disp, md.Output())}}); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(g.GRPCStreamHandler()))
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mctx := metadata.AppendToOutgoingContext(ctx, mdSubscribeHMAC, "mac0", mdSubscribeTimestamp, "100", mdSubscribeKid, "k0")
	stream, err := greeterv1.NewGreeterServiceClient(conn).Greetings(mctx, &greeterv1.GreetingsFilter{Name: "grpc"})
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := stream.Recv(); err != nil || ev.GetForName() != "grpc" {
		t.Fatalf("grpc event: %v %v", ev, err)
	}

	url := serveConnect(t, g.GRPCHandler())
	for i, opts := range [][]connect.ClientOption{nil, {connect.WithGRPCWeb()}} {
		c := connect.NewClient[greeterv1.GreetingsFilter, greeterv1.Greeting](http.DefaultClient, url+"/greeter.v1.GreeterService/Greetings", opts...)
		req := connect.NewRequest(&greeterv1.GreetingsFilter{Name: "connect"})
		req.Header().Set(mdSubscribeHMAC, fmt.Sprint("mac", i+1))
		req.Header().Set(mdSubscribeTimestamp, strconv.Itoa(101+i))
		req.Header().Set(mdSubscribeKid, fmt.Sprint("k", i+1))
		st, err := c.CallServerStream(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Receive() || st.Msg().GetForName() != "connect" {
			t.Fatalf("connect event %d: %v", i, st.Err())
		}
		_ = st.Close()
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.args) != 3 {
		t.Fatalf("dispatches = %d, want 3", len(disp.args))
	}
	for i, a := range disp.args {
		want := fmt.Sprintf("mac%d %d k%d", i, 100+i, i)
		if got := fmt.Sprint(a["hmac"], " ", a["timestamp"], " ", a["kid"]); got != want {
			t.Errorf("dispatch %d: auth args %q, want %q", i, got, want)
		}
	}
}

// Changed with the move onto gat's gRPC ingress: an upstream HTTP
// error status picks the gRPC code (gat.ErrorCode). Was Internal for
// every 5xx and InvalidArgument for 409.
func TestGRPCIngress_UpstreamHTTPStatusCode(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(r.URL.Path[len("/things/"):])
		w.WriteHeader(code)
	}))
	defer backend.Close()
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test")))
	defer gw.Close()
	if err := gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(backend.URL), As("test")); err != nil {
		t.Fatal(err)
	}
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
	inDesc, outDesc := (&crossFormatFixture{gw: gw}).synthMethodDescriptors(t, "test", "v1", "getThing")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for upstream, want := range map[int]codes.Code{
		http.StatusNotFound:            codes.NotFound,
		http.StatusConflict:            codes.AlreadyExists,
		http.StatusTooManyRequests:     codes.ResourceExhausted,
		http.StatusInternalServerError: codes.Internal,
		http.StatusServiceUnavailable:  codes.Unavailable,
	} {
		in := dynamicpb.NewMessage(inDesc)
		in.Set(inDesc.Fields().ByName("id"), protoreflect.ValueOfString(strconv.Itoa(upstream)))
		err := conn.Invoke(ctx, "/test.v1.Service/getThing", in, dynamicpb.NewMessage(outDesc))
		if status.Code(err) != want {
			t.Errorf("upstream %d: %v, want %v", upstream, err, want)
		}
	}
}

var _ ir.Dispatcher = (*recordingDispatcher)(nil)

// A Connect call gets the gateway.grpc span, continuing the trace in
// its traceparent header.
func TestTracing_ConnectIngressSpan(t *testing.T) {
	gw, exp, _ := newTracingFixture(t)
	url := serveConnect(t, gw.ConnectHandler())
	const traceID = "0af7651916cd43dd8448eb211c80319c"
	c := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](http.DefaultClient, url+"/greeter.v1.GreeterService/Hello")
	req := connect.NewRequest(&greeterv1.HelloRequest{Name: "world"})
	req.Header().Set("traceparent", "00-"+traceID+"-b7ad6b7169203331-01")
	if _, err := c.CallUnary(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ingress := findSpan(exp.GetSpans(), "gateway.grpc")
	if ingress == nil {
		t.Fatalf("no gateway.grpc span; got=%v", spanNamesFor(exp.GetSpans()))
	}
	if got := spanAttr(ingress, attrNamespace); got != "greeter" {
		t.Errorf("gateway.namespace=%q want=greeter", got)
	}
	if got := ingress.SpanContext.TraceID().String(); got != traceID {
		t.Errorf("trace id %s, want %s", got, traceID)
	}
}
