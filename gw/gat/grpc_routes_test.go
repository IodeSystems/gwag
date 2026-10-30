package gat_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// routeService is a hand-built descriptor: Hello (unary), Watch
// (server-streaming), Upload (client-streaming).
func routeService(t *testing.T) protoreflect.ServiceDescriptor {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("routes_test.proto"),
		Package: proto.String("routes.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Req"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("name"), Number: proto.Int32(1), Type: str, Label: opt, JsonName: proto.String("name")}}},
			{Name: proto.String("Resp"), Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("greeting"), Number: proto.Int32(1), Type: str, Label: opt, JsonName: proto.String("greeting")}}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Svc"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("Hello"), InputType: proto.String(".routes.v1.Req"), OutputType: proto.String(".routes.v1.Resp")},
				{Name: proto.String("Watch"), InputType: proto.String(".routes.v1.Req"), OutputType: proto.String(".routes.v1.Resp"), ServerStreaming: proto.Bool(true)},
				{Name: proto.String("Upload"), InputType: proto.String(".routes.v1.Req"), OutputType: proto.String(".routes.v1.Resp"), ClientStreaming: proto.Bool(true)},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Services().Get(0)
}

func getString(m protoreflect.ProtoMessage, field string) string {
	r := m.ProtoReflect()
	return r.Get(r.Descriptor().Fields().ByName(protoreflect.Name(field))).String()
}

func respWith(md protoreflect.MethodDescriptor, greeting string) *dynamicpb.Message {
	out := dynamicpb.NewMessage(md.Output())
	out.Set(md.Output().Fields().ByName("greeting"), protoreflect.ValueOfString(greeting))
	return out
}

// helloRoutes greets with prefix + name (+ the x-who header when sent),
// streams three greetings on Watch, and counts released responses.
func helloRoutes(sd protoreflect.ServiceDescriptor, prefix string, released *atomic.Int32) []gat.GRPCRoute {
	hello, watch := sd.Methods().ByName("Hello"), sd.Methods().ByName("Watch")
	return []gat.GRPCRoute{
		{
			Method: hello,
			Data:   "hello-data",
			Unary: func(ctx context.Context, req *dynamicpb.Message) (proto.Message, error) {
				name := getString(req, "name")
				if name == "fail" {
					return nil, &statusErr{status: http.StatusConflict, msg: "taken"}
				}
				if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("x-who")) > 0 {
					name += "/" + md.Get("x-who")[0]
				}
				return respWith(hello, prefix+name), nil
			},
			Release: func(proto.Message) { released.Add(1) },
		},
		{
			Method: watch,
			Stream: func(ctx context.Context, req *dynamicpb.Message, send func(proto.Message) error) error {
				for i := range 3 {
					if err := send(respWith(watch, fmt.Sprintf("%s%s %d", prefix, getString(req, "name"), i))); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
}

type statusErr struct {
	status int
	msg    string
}

func (e *statusErr) Error() string  { return e.msg }
func (e *statusErr) GetStatus() int { return e.status }

func startGRPCRoutes(t *testing.T, g *gat.Gateway) (grpcConn *grpc.ClientConn, connectURL string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(g.GRPCStreamHandler()))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	mux := http.NewServeMux()
	mux.Handle("/rpc/", http.StripPrefix("/rpc", g.GRPCHandler()))
	hs := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	hs.EnableHTTP2 = true
	hs.Start()
	t.Cleanup(hs.Close)
	return conn, hs.URL + "/rpc"
}

func connectClient(url string, md protoreflect.MethodDescriptor, opts ...connect.ClientOption) *connect.Client[dynamicpb.Message, dynamicpb.Message] {
	opts = append(opts,
		connect.WithSchema(md),
		connect.WithResponseInitializer(func(_ connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(md.Output())
			return nil
		}))
	return connect.NewClient[dynamicpb.Message, dynamicpb.Message](http.DefaultClient, url, opts...)
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}

func reqWith(md protoreflect.MethodDescriptor, name string) *dynamicpb.Message {
	in := dynamicpb.NewMessage(md.Input())
	in.Set(md.Input().Fields().ByName("name"), protoreflect.ValueOfString(name))
	return in
}

func TestGRPCRoutes_AllTransports(t *testing.T) {
	sd := routeService(t)
	hello, watch := sd.Methods().ByName("Hello"), sd.Methods().ByName("Watch")
	var released atomic.Int32
	g := mustNewGat(t)
	if err := g.SetGRPCRoutes(helloRoutes(sd, "hi ", &released)); err != nil {
		t.Fatal(err)
	}
	conn, url := startGRPCRoutes(t, g)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// grpc-go server.
	out := dynamicpb.NewMessage(hello.Output())
	mctx := metadata.AppendToOutgoingContext(ctx, "x-who", "grpc")
	if err := conn.Invoke(mctx, "/routes.v1.Svc/Hello", reqWith(hello, "ann"), out); err != nil {
		t.Fatalf("grpc unary: %v", err)
	}
	if got := getString(out, "greeting"); got != "hi ann/grpc" {
		t.Errorf("grpc unary: %q", got)
	}
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/routes.v1.Svc/Watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(reqWith(watch, "bo")); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	var got []string
	for {
		m := dynamicpb.NewMessage(watch.Output())
		if err := stream.RecvMsg(m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("grpc stream: %v", err)
		}
		got = append(got, getString(m, "greeting"))
	}
	if fmt.Sprint(got) != "[hi bo 0 hi bo 1 hi bo 2]" {
		t.Errorf("grpc stream: %v", got)
	}

	// Connect, gRPC and gRPC-Web over GRPCHandler.
	for name, opts := range map[string][]connect.ClientOption{
		"connect":      nil,
		"connect-json": {connect.WithProtoJSON()},
		"grpc-web":     {connect.WithGRPCWeb()},
	} {
		c := connectClient(url+"/routes.v1.Svc/Hello", hello, opts...)
		req := connect.NewRequest(reqWith(hello, "cy"))
		req.Header().Set("X-Who", name)
		resp, err := c.CallUnary(ctx, req)
		if err != nil {
			t.Fatalf("%s unary: %v", name, err)
		}
		if got := getString(resp.Msg, "greeting"); got != "hi cy/"+name {
			t.Errorf("%s unary: %q", name, got)
		}
		sc := connectClient(url+"/routes.v1.Svc/Watch", watch, opts...)
		st, err := sc.CallServerStream(ctx, connect.NewRequest(reqWith(watch, "di")))
		if err != nil {
			t.Fatalf("%s stream: %v", name, err)
		}
		n := 0
		for st.Receive() {
			n++
		}
		if st.Err() != nil || n != 3 {
			t.Errorf("%s stream: %d messages, err %v", name, n, st.Err())
		}
	}
	grpcOverHTTP := connect.NewClient[dynamicpb.Message, dynamicpb.Message](h2cClient(), url+"/routes.v1.Svc/Hello",
		connect.WithGRPC(), connect.WithSchema(hello),
		connect.WithResponseInitializer(func(_ connect.Spec, msg any) error {
			*msg.(*dynamicpb.Message) = *dynamicpb.NewMessage(hello.Output())
			return nil
		}))
	if resp, err := grpcOverHTTP.CallUnary(ctx, connect.NewRequest(reqWith(hello, "ed"))); err != nil || getString(resp.Msg, "greeting") != "hi ed" {
		t.Errorf("grpc over GRPCHandler: %v %v", resp, err)
	}

	// The binary codecs released their responses; the JSON one did not.
	if n := released.Load(); n != 4 {
		t.Errorf("released %d responses, want 4 (grpc-go, connect, grpc-web, grpc)", n)
	}

	// Errors map by ErrorCode on both servers.
	err = conn.Invoke(ctx, "/routes.v1.Svc/Hello", reqWith(hello, "fail"), dynamicpb.NewMessage(hello.Output()))
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("grpc error: %v", err)
	}
	_, err = connectClient(url+"/routes.v1.Svc/Hello", hello).CallUnary(ctx, connect.NewRequest(reqWith(hello, "fail")))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("connect error: %v", err)
	}
	// Unknown and client-streaming methods are not routed.
	if err := conn.Invoke(ctx, "/routes.v1.Svc/Nope", reqWith(hello, "x"), out); status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown method: %v", err)
	}
}

func TestGRPCRoutes_PreCall(t *testing.T) {
	sd := routeService(t)
	hello := sd.Methods().ByName("Hello")
	var released, done atomic.Int32
	var refuse atomic.Bool
	var seen atomic.Value
	g := mustNewGat(t).PreCall(func(ctx context.Context, call gat.GRPCCall) (context.Context, func(context.Context), error) {
		md, _ := metadata.FromIncomingContext(ctx)
		seen.Store(fmt.Sprint(call.Procedure, " ", call.Streaming, " ", call.Data, " ", md.Get("x-who")))
		finish := func(context.Context) { done.Add(1) }
		if refuse.Load() {
			return ctx, finish, status.Error(codes.Unavailable, "draining")
		}
		return ctx, finish, nil
	})
	if err := g.SetGRPCRoutes(helloRoutes(sd, "hi ", &released)); err != nil {
		t.Fatal(err)
	}
	conn, url := startGRPCRoutes(t, g)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-who", "z")
	out := dynamicpb.NewMessage(hello.Output())

	if err := conn.Invoke(ctx, "/routes.v1.Svc/Hello", reqWith(hello, "a"), out); err != nil {
		t.Fatal(err)
	}
	if s := seen.Load(); s != "/routes.v1.Svc/Hello false hello-data [z]" {
		t.Errorf("hook saw %v", s)
	}
	c := connectClient(url+"/routes.v1.Svc/Hello", hello)
	req := connect.NewRequest(reqWith(hello, "a"))
	req.Header().Set("X-Who", "z")
	if _, err := c.CallUnary(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if s := seen.Load(); s != "/routes.v1.Svc/Hello false hello-data [z]" {
		t.Errorf("hook saw over connect %v", s)
	}

	refuse.Store(true)
	if err := conn.Invoke(ctx, "/routes.v1.Svc/Hello", reqWith(hello, "a"), out); status.Code(err) != codes.Unavailable {
		t.Errorf("refused grpc call: %v", err)
	}
	if _, err := c.CallUnary(context.Background(), connect.NewRequest(reqWith(hello, "a"))); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("refused connect call: %v", err)
	}
	if n := done.Load(); n != 4 {
		t.Errorf("done ran %d times, want 4 (refused calls included)", n)
	}
	if n := released.Load(); n != 2 {
		t.Errorf("released %d, want 2: refused calls have no response", n)
	}
}

// PreCall wraps gat's own operations too.
func TestGRPCRoutes_PreCallOnOwnOperations(t *testing.T) {
	var seen atomic.Value
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("Demo", "1.0.0"))
	g := mustNewGat(t).PreCall(func(ctx context.Context, call gat.GRPCCall) (context.Context, func(context.Context), error) {
		md, _ := metadata.FromIncomingContext(ctx)
		seen.Store(call.Procedure + " " + fmt.Sprint(md.Get("x-who")))
		return ctx, nil, status.Error(codes.PermissionDenied, "no")
	})
	gat.Register(api, g, huma.Operation{OperationID: "getProject", Method: http.MethodGet, Path: "/projects/{id}"},
		func(ctx context.Context, in *getProjectInput) (*getProjectOutput, error) {
			t.Error("handler ran on a refused call")
			return nil, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(mustGET(t, srv.URL+"/api/schema/proto"), fds); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	md := findMethod(t, files, "getProject")
	proc := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
	req := connect.NewRequest(dynamicpb.NewMessage(md.Input()))
	req.Header().Set("X-Who", "me")
	_, err = connectClient(srv.URL+"/api/grpc"+proc, md).CallUnary(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("refused call: %v", err)
	}
	if s := seen.Load(); s != proc+" [me]" {
		t.Errorf("hook saw %v", s)
	}
}

// A route table swapped mid-call leaves that call on its old route.
func TestGRPCRoutes_SwapIsAtomic(t *testing.T) {
	sd := routeService(t)
	hello := sd.Methods().ByName("Hello")
	var released atomic.Int32
	entered, unblock := make(chan struct{}), make(chan struct{})
	old := []gat.GRPCRoute{{
		Method: hello,
		Unary: func(ctx context.Context, req *dynamicpb.Message) (proto.Message, error) {
			close(entered)
			<-unblock
			return respWith(hello, "old "+getString(req, "name")), nil
		},
	}}
	g := mustNewGat(t)
	if err := g.SetGRPCRoutes(old); err != nil {
		t.Fatal(err)
	}
	conn, _ := startGRPCRoutes(t, g)
	ctx := context.Background()
	res := make(chan string, 1)
	go func() {
		out := dynamicpb.NewMessage(hello.Output())
		if err := conn.Invoke(ctx, "/routes.v1.Svc/Hello", reqWith(hello, "a"), out); err != nil {
			res <- err.Error()
			return
		}
		res <- getString(out, "greeting")
	}()
	<-entered
	if err := g.SetGRPCRoutes(helloRoutes(sd, "new ", &released)); err != nil {
		t.Fatal(err)
	}
	out := dynamicpb.NewMessage(hello.Output())
	if err := conn.Invoke(ctx, "/routes.v1.Svc/Hello", reqWith(hello, "b"), out); err != nil || getString(out, "greeting") != "new b" {
		t.Errorf("call after the swap: %v %v", out, err)
	}
	close(unblock)
	if got := <-res; got != "old a" {
		t.Errorf("in-flight call: %q", got)
	}
}

func TestGRPCRoutes_Refused(t *testing.T) {
	sd := routeService(t)
	hello, watch, upload := sd.Methods().ByName("Hello"), sd.Methods().ByName("Watch"), sd.Methods().ByName("Upload")
	unary := func(context.Context, *dynamicpb.Message) (proto.Message, error) { return nil, nil }
	stream := func(context.Context, *dynamicpb.Message, func(proto.Message) error) error { return nil }
	g := mustNewGat(t)
	for name, routes := range map[string][]gat.GRPCRoute{
		"no method":        {{Unary: unary}},
		"client streaming": {{Method: upload, Unary: unary}},
		"unary as stream":  {{Method: hello, Stream: stream}},
		"stream as unary":  {{Method: watch, Unary: unary}},
		"duplicate":        {{Method: hello, Unary: unary}, {Method: hello, Unary: unary}},
	} {
		if err := g.SetGRPCRoutes(routes); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestErrorCode(t *testing.T) {
	cases := []struct {
		err  error
		want connect.Code
	}{
		{connect.NewError(connect.CodeAborted, errors.New("x")), connect.CodeAborted},
		{status.Error(codes.OutOfRange, "x"), connect.CodeOutOfRange},
		{fmt.Errorf("wrapped: %w", status.Error(codes.NotFound, "x")), connect.CodeNotFound},
		{&statusErr{status: http.StatusServiceUnavailable}, connect.CodeUnavailable},
		{fmt.Errorf("wrapped: %w", &statusErr{status: http.StatusConflict}), connect.CodeAlreadyExists},
		{context.Canceled, connect.CodeCanceled},
		{fmt.Errorf("call: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded},
		{errors.New("boom"), connect.CodeInternal},
	}
	for _, c := range cases {
		if got := gat.ErrorCode(c.err); got != c.want {
			t.Errorf("ErrorCode(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
