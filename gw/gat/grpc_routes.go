package gat

// Host routes: methods a host serves on gat's gRPC ingress besides
// gat's own operations. gat owns the transports (Connect, gRPC and
// gRPC-Web on an http.Handler; gRPC on a grpc-go server through
// GRPCStreamHandler), the request messages, the PreCall hook and the
// error mapping; the host owns what a call does. SetGRPCRoutes swaps
// the whole table at once, so a host that registers services at run
// time can rebuild it without disturbing calls in flight.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// GRPCRoute is one method a host serves on gat's gRPC ingress (see
// SetGRPCRoutes). Set Unary for a unary method, Stream for a
// server-streaming one.
//
// Stability: experimental
type GRPCRoute struct {
	// Method names the procedure (/package.Service/Method) and gives
	// the request and response types.
	Method protoreflect.MethodDescriptor

	// Unary answers a unary call. req belongs to gat and is valid
	// until Unary returns.
	Unary func(ctx context.Context, req *dynamicpb.Message) (proto.Message, error)

	// Stream answers a server-streaming call, sending each response
	// with send; the stream ends when Stream returns. req is valid
	// until Stream returns. A message passed to send may be reused once
	// send returns.
	Stream func(ctx context.Context, req *dynamicpb.Message, send func(proto.Message) error) error

	// Release, when set, receives a unary response once gat has
	// encoded it, so the host can pool response messages. A response
	// gat cannot tell is done with (the Connect JSON codec, a failed
	// call) is left to the garbage collector instead.
	Release func(proto.Message)

	// Data is handed to the PreCall hook as GRPCCall.Data. gat does
	// not read it.
	Data any
}

// GRPCCall describes a gRPC call to the PreCall hook.
//
// Stability: experimental
type GRPCCall struct {
	// Procedure is /package.Service/Method.
	Procedure string
	// Streaming is set for a server-streaming call.
	Streaming bool
	// Data is the host route's GRPCRoute.Data; nil for gat's own
	// operations.
	Data any
}

// CallHook runs before gat answers a gRPC call. It returns the context
// the call runs with, and done, called (if non-nil) with that context
// when the call ends. A non-nil error refuses the call: the client
// gets that error, its code from ErrorCode, and only done runs.
//
// Stability: experimental
type CallHook func(ctx context.Context, call GRPCCall) (context.Context, func(context.Context), error)

// PreCall sets a hook that runs for every gRPC, gRPC-Web and Connect
// call (gat's own operations and host routes) once the request message
// is read. The call's metadata is on ctx as incoming gRPC metadata
// (metadata.FromIncomingContext) on every transport. Use it to refuse
// calls while draining, start a trace span, take a concurrency slot
// and record the call in done. nil disables it. Set it before serving.
// Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) PreCall(fn CallHook) *Gateway {
	g.preCall = fn
	return g
}

// hostRoute is a GRPCRoute ready to serve.
type hostRoute struct {
	GRPCRoute
	call     GRPCCall
	requests *sync.Pool   // request messages of Method.Input()
	connect  http.Handler // the route over Connect, gRPC and gRPC-Web
}

// grpcRouteTable is one SetGRPCRoutes generation: immutable once
// stored.
type grpcRouteTable struct {
	routes map[string]*hostRoute // by procedure
}

// SetGRPCRoutes replaces the host routes served by GRPCHandler and
// GRPCStreamHandler. The swap is atomic: a call already running
// finishes on the route it started with. Client-streaming methods are
// refused, as is a route whose handler does not match its method's
// shape or a procedure listed twice; on error the previous routes
// stay. Safe to call while serving.
//
// Stability: experimental
func (g *Gateway) SetGRPCRoutes(routes []GRPCRoute) error {
	t := &grpcRouteTable{routes: make(map[string]*hostRoute, len(routes))}
	for _, r := range routes {
		if r.Method == nil {
			return errors.New("gat: GRPCRoute without a Method")
		}
		md := r.Method
		proc := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
		switch {
		case md.IsStreamingClient():
			return fmt.Errorf("gat: %s: client-streaming methods are not supported", proc)
		case md.IsStreamingServer() && (r.Stream == nil || r.Unary != nil):
			return fmt.Errorf("gat: %s: a server-streaming method takes Stream only", proc)
		case !md.IsStreamingServer() && (r.Unary == nil || r.Stream != nil):
			return fmt.Errorf("gat: %s: a unary method takes Unary only", proc)
		}
		if _, dup := t.routes[proc]; dup {
			return fmt.Errorf("gat: %s: listed twice", proc)
		}
		in := md.Input()
		hr := &hostRoute{
			GRPCRoute: r,
			call:      GRPCCall{Procedure: proc, Streaming: md.IsStreamingServer(), Data: r.Data},
			requests:  &sync.Pool{New: func() any { return dynamicpb.NewMessage(in) }},
		}
		hr.connect = g.hostConnectHandler(hr)
		t.routes[proc] = hr
	}
	g.grpcRoutes.Store(t)
	return nil
}

func (r *hostRoute) getRequest() *dynamicpb.Message {
	return r.requests.Get().(*dynamicpb.Message)
}

func (r *hostRoute) putRequest(m *dynamicpb.Message) {
	proto.Reset(m)
	r.requests.Put(m)
}

// GRPCHandler serves the host routes over Connect, gRPC and gRPC-Web
// (connect-go's protocols) at /package.Service/Method. Mount it where
// those paths arrive, with http.StripPrefix for a prefix; native gRPC
// needs HTTP/2 (h2c or TLS). An unknown procedure answers 404.
//
// Stability: experimental
func (g *Gateway) GRPCHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := g.grpcRoutes.Load(); t != nil {
			if hr := t.routes[r.URL.Path]; hr != nil {
				hr.connect.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
}

// GRPCStreamHandler serves the host routes on a grpc-go server, for
// grpc.UnknownServiceHandler. An unknown method answers Unimplemented.
//
// Stability: experimental
func (g *Gateway) GRPCStreamHandler() grpc.StreamHandler {
	return g.serveGRPCStream
}

func (g *Gateway) serveGRPCStream(_ any, stream grpc.ServerStream) error {
	proc, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "gat: missing method on stream")
	}
	t := g.grpcRoutes.Load()
	if t == nil {
		return status.Error(codes.Unimplemented, "gat: no gRPC routes")
	}
	hr := t.routes[proc]
	if hr == nil {
		return status.Errorf(codes.Unimplemented, "gat: no route for %s", proc)
	}
	req := hr.getRequest()
	defer hr.putRequest(req)
	if err := stream.RecvMsg(req); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return status.Errorf(codes.InvalidArgument, "gat: decode request: %v", err)
	}
	ctx := stream.Context()
	var done func(context.Context)
	if g.preCall != nil {
		var err error
		ctx, done, err = g.preCall(ctx, hr.call)
		if done != nil {
			defer done(ctx)
		}
		if err != nil {
			return grpcStatusError(err)
		}
	}
	if hr.Stream != nil {
		return grpcStatusError(hr.Stream(ctx, req, func(m proto.Message) error { return stream.SendMsg(m) }))
	}
	resp, err := hr.Unary(ctx, req)
	if err != nil {
		return grpcStatusError(err)
	}
	err = stream.SendMsg(resp)
	if hr.Release != nil {
		hr.Release(resp)
	}
	return err
}

// hostRequest is the value connect decodes a host route's request
// into: a pooled dynamicpb message, set by the route's request
// initializer.
type hostRequest struct {
	msg *dynamicpb.Message
}

func (r *hostRequest) ProtoReflect() protoreflect.Message { return r.msg.ProtoReflect() }

// hostResult is a host route's response on the Connect transports.
// fastProtoCodec hands it to release once encoded.
type hostResult struct {
	msg     proto.Message
	release func(proto.Message)
}

func (r *hostResult) ProtoReflect() protoreflect.Message { return r.msg.ProtoReflect() }

func (g *Gateway) hostConnectHandler(hr *hostRoute) http.Handler {
	md := hr.Method
	opts := []connect.HandlerOption{
		connect.WithSchema(md),
		connect.WithRequestInitializer(func(_ connect.Spec, msg any) error {
			if r, ok := msg.(*hostRequest); ok {
				r.msg = hr.getRequest()
			}
			return nil
		}),
		connect.WithCodec(fastProtoCodec{}),
	}
	if hr.Stream != nil {
		return connect.NewServerStreamHandler(hr.call.Procedure,
			func(ctx context.Context, req *connect.Request[hostRequest], stream *connect.ServerStream[hostResult]) error {
				defer hr.putRequest(req.Msg.msg)
				ctx = withHeaderMetadata(ctx, req.Header())
				ctx, done, err := g.runPreCall(ctx, hr.call)
				if done != nil {
					defer done(ctx)
				}
				if err != nil {
					return connectError(err)
				}
				send := func(m proto.Message) error { return stream.Send(&hostResult{msg: m}) }
				if err := hr.Stream(ctx, req.Msg.msg, send); err != nil {
					return connectError(err)
				}
				return nil
			}, opts...)
	}
	return connect.NewUnaryHandler(hr.call.Procedure,
		func(ctx context.Context, req *connect.Request[hostRequest]) (*connect.Response[hostResult], error) {
			defer hr.putRequest(req.Msg.msg)
			ctx = withHeaderMetadata(ctx, req.Header())
			ctx, done, err := g.runPreCall(ctx, hr.call)
			if done != nil {
				defer done(ctx)
			}
			if err != nil {
				return nil, connectError(err)
			}
			resp, err := hr.Unary(ctx, req.Msg.msg)
			if err != nil {
				return nil, connectError(err)
			}
			return connect.NewResponse(&hostResult{msg: resp, release: hr.Release}), nil
		}, opts...)
}

// runPreCall runs the PreCall hook when one is set.
func (g *Gateway) runPreCall(ctx context.Context, call GRPCCall) (context.Context, func(context.Context), error) {
	if g.preCall == nil {
		return ctx, nil, nil
	}
	return g.preCall(ctx, call)
}

// withHeaderMetadata puts a Connect call's headers on ctx as incoming
// gRPC metadata, the shape a grpc-go server gives its handlers: keys
// lowercased, -bin values base64-decoded.
func withHeaderMetadata(ctx context.Context, h http.Header) context.Context {
	md := make(metadata.MD, len(h))
	for k, vs := range h {
		k = strings.ToLower(k)
		if strings.HasSuffix(k, "-bin") {
			for _, v := range vs {
				if b, err := connect.DecodeBinaryHeader(v); err == nil {
					md[k] = append(md[k], string(b))
				}
			}
			continue
		}
		md[k] = append(md[k], vs...)
	}
	return metadata.NewIncomingContext(ctx, md)
}

// ErrorCode classifies err as the gRPC / Connect code a client gets
// for it, on every gRPC transport gat serves:
//
//   - a *connect.Error, or an error with a gRPC status
//     (status.FromError), keeps its code;
//   - an error with an HTTP status (a GetStatus() int method, as huma's
//     errors have) maps by status: 400 InvalidArgument, 401
//     Unauthenticated, 403 PermissionDenied, 404 and 410 NotFound, 409
//     AlreadyExists, 412 FailedPrecondition, 413 and 429
//     ResourceExhausted, 501 Unimplemented, 502, 503 and 504
//     Unavailable, other 4xx InvalidArgument, other 5xx Internal;
//   - context.Canceled and context.DeadlineExceeded map to Canceled and
//     DeadlineExceeded;
//   - anything else is Internal.
//
// Wrapped errors are unwrapped. err must not be nil.
//
// Stability: experimental
func ErrorCode(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	if st, ok := status.FromError(err); ok {
		return connect.Code(st.Code())
	}
	type statusError interface{ GetStatus() int }
	var se statusError
	if errors.As(err, &se) {
		return httpStatusToConnectCode(se.GetStatus())
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return connect.CodeDeadlineExceeded
	}
	return connect.CodeInternal
}

// connectError is err as the *connect.Error a Connect handler returns.
func connectError(err error) *connect.Error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	if st, ok := status.FromError(err); ok {
		return connect.NewError(connect.Code(st.Code()), errors.New(st.Message()))
	}
	return connect.NewError(ErrorCode(err), err)
}

// grpcStatusError is err as the status error a grpc-go handler
// returns.
func grpcStatusError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return status.Error(codes.Code(ce.Code()), ce.Message())
	}
	return status.Error(codes.Code(ErrorCode(err)), err.Error())
}
