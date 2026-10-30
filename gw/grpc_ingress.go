package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/ir"
)

// gRPC ingress runs on gat's (gat.SetGRPCRoutes): gat owns the
// transports (grpc-go for GRPCUnknownHandler; Connect, gRPC and
// gRPC-Web for ConnectHandler), the request messages and the error
// mapping (gat.ErrorCode). gwag supplies the routes, rebuilt on every
// assembleLocked, and grpcPreCall, which wraps each call with drain,
// tracing, metrics, the request log and per-pool backpressure.

// grpcRouteInfo is what grpcPreCall needs to know about a route:
// carried as gat.GRPCRoute.Data.
type grpcRouteInfo struct {
	namespace, version string
	// bp is the pool's backpressure for unary proto routes; zero (no
	// Sem) for in-process and canonical-args routes, whose dispatchers
	// carry their own.
	bp backpressureConfig
}

// noRouteInfo stands in for a call without route info (gat's own
// operations; gwag registers none).
var noRouteInfo = &grpcRouteInfo{}

// rebuildGRPCIngressLocked walks every proto pool's RPCs and emits
// one route per method. Unary routes run the same chained Handler the
// canonical protoDispatcher uses (so transforms apply identically);
// grpcPreCall takes the per-pool backpressure slot. Server-streaming
// routes run the protoDirectSubscriptionDispatcher already registered
// in g.dispatchers — it opens a direct gRPC stream to the upstream per
// subscriber.
//
// Bidi / client-streaming methods are skipped — egress doesn't
// support them and there's no canonical args shape to forward.
// Internal namespaces (`_*` or AsInternal) skip just like the
// GraphQL surface skips them.
//
// After the proto walk, a second pass synthesizes routes for
// non-proto slots (slotKindOpenAPI, slotKindGraphQL) from
// `ir.RenderProtoFiles` so a gRPC client can reach OpenAPI- or
// GraphQL-registered ops too. Synthesized routes go through the
// canonical-args dispatcher (slower than the proto-on-proto fast
// path; fine for cross-kind which won't be hot).
//
// The routes go to gat in one SetGRPCRoutes: calls in flight finish
// on the table they started with.
//
// Caller holds g.mu.
func (g *Gateway) rebuildGRPCIngressLocked() error {
	chain := g.runtimeChain()
	headers := g.headerInjectorSnapshot()
	metrics := g.cfg.metrics
	bpOpts := g.cfg.backpressure
	var routes []gat.GRPCRoute
	seen := map[string]bool{}
	add := func(r gat.GRPCRoute) {
		path := methodLabel(r.Method.Parent().(protoreflect.ServiceDescriptor), r.Method)
		if seen[path] {
			return
		}
		seen[path] = true
		routes = append(routes, r)
	}
	for _, slot := range g.slots {
		switch slot.kind {
		case slotKindProto:
			p := slot.proto
			if g.isInternal(p.key.namespace) {
				continue
			}
			services := p.file.Services()
			for i := 0; i < services.Len(); i++ {
				sd := services.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					md := methods.Get(j)
					if md.IsStreamingClient() {
						// bidi/client-streaming — egress can't synthesise
						// these and ingress has no canonical-args shape.
						continue
					}
					info := &grpcRouteInfo{namespace: p.key.namespace, version: p.key.version}
					if md.IsStreamingServer() {
						sid := ir.MakeSchemaID(p.key.namespace, p.key.version, string(md.Name()))
						disp := g.dispatchers.Get(sid)
						if disp == nil {
							continue
						}
						add(gat.GRPCRoute{Method: md, Stream: grpcSubscription(disp, md.Output()), Data: info})
						continue
					}
					inner := newProtoInvocationHandler(p, sd, md, headers, metrics, bpOpts)
					info.bp = poolBackpressureConfig(p, methodLabel(sd, md), metrics, bpOpts)
					add(grpcUnaryRoute(md, chain(inner), info))
				}
			}
		case slotKindInternalProto:
			src := slot.internalProto
			if g.isInternal(slot.key.namespace) {
				continue
			}
			services := src.file.Services()
			for i := 0; i < services.Len(); i++ {
				sd := services.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					md := methods.Get(j)
					if md.IsStreamingClient() || md.IsStreamingServer() {
						// Subscription path is broker-driven (see
						// registerProtoDispatchersLocked); gRPC
						// ingress for in-process subscriptions
						// lands when the PubSub handler does.
						continue
					}
					inner := newInternalProtoInvocationHandler(src, sd, md, metrics)
					// No pool / backpressure for in-process dispatch.
					add(grpcUnaryRoute(md, chain(inner), noRouteInfo))
				}
			}
		}
	}

	// Cross-kind second pass: synthesize routes for openapi /
	// graphql slots via ir.RenderProtoFiles. The dispatcher
	// already includes backpressureMiddleware (see
	// openapi_register / graphql_register) so the route carries no
	// backpressure of its own.
	for k, slot := range g.slots {
		if slot.kind == slotKindProto || slot.kind == slotKindInternalProto {
			continue
		}
		if g.isInternal(k.namespace) {
			continue
		}
		g.addCanonicalArgsGRPCRoutesLocked(add, k, slot.ir)
	}

	return g.gql.SetGRPCRoutes(routes)
}

// grpcUnaryRoute serves a unary method with a chained Handler. The
// handler's response comes from acquireDynamicMessage; gat hands it
// back through Release once encoded.
func grpcUnaryRoute(md protoreflect.MethodDescriptor, h Handler, info *grpcRouteInfo) gat.GRPCRoute {
	out := md.Output()
	return gat.GRPCRoute{
		Method: md,
		Unary: func(ctx context.Context, req *dynamicpb.Message) (proto.Message, error) {
			return h(ctx, req)
		},
		Release: func(m proto.Message) {
			if dm, ok := m.(*dynamicpb.Message); ok {
				releaseDynamicMessage(out, dm)
			}
		},
		Data: info,
	}
}

// addCanonicalArgsGRPCRoutesLocked synthesizes input/output
// MessageDescriptors per non-proto service via ir.RenderProtoFiles
// and emits routes that bridge dynamicpb ↔ canonical-args
// dispatcher: messageToMap on the input → ir.Dispatcher.Dispatch
// → argsToMessage onto the synthesized response message (which has
// shape `{value: <output>}` per ir.synthResponseMessage). Streaming
// methods are skipped — graphql subscriptions and openapi
// long-poll-as-stream don't have a stable canonical-args bridge
// from the gRPC side yet. Caller holds g.mu.
func (g *Gateway) addCanonicalArgsGRPCRoutesLocked(add func(gat.GRPCRoute), key poolKey, services []*ir.Service) {
	for _, svc := range services {
		fds, err := ir.RenderProtoFiles([]*ir.Service{svc})
		if err != nil || fds == nil || len(fds.File) == 0 {
			continue
		}
		files, err := protodesc.NewFiles(fds)
		if err != nil {
			continue
		}
		for _, fp := range fds.File {
			fd, err := files.FindFileByPath(fp.GetName())
			if err != nil {
				continue
			}
			ss := fd.Services()
			for i := 0; i < ss.Len(); i++ {
				sd := ss.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					md := methods.Get(j)
					if md.IsStreamingClient() || md.IsStreamingServer() {
						continue
					}
					sid := ir.MakeSchemaID(key.namespace, key.version, string(md.Name()))
					disp := g.dispatchers.Get(sid)
					if disp == nil {
						continue
					}
					// A path a proto slot already serves keeps that route.
					add(grpcUnaryRoute(md, newCanonicalArgsGRPCHandler(disp, md.Output()), noRouteInfo))
				}
			}
		}
	}
}

// newCanonicalArgsGRPCHandler builds a Handler that bridges
// dynamicpb ↔ canonical-args ir.Dispatcher: the input dynamicpb
// converts to args via messageToMap, the dispatcher returns a
// scalar / map / list / nil, and the result is either passed
// through to argsToMessage (when outputDesc is the user-facing
// response shape — non-repeated Named output) or wrapped as
// `{value: <result>}` (when outputDesc is the synthesised wrapper
// emitted by ir.synthResponseMessage for primitive / repeated /
// nil-output ops).
//
// The wrap-or-pass-through decision is a structural property of
// outputDesc: a wrapper always has exactly one field named "value"
// (cf. ir.synthResponseMessage). That signal's robust against
// user-facing types that happen to have a "value" field — they'd
// be a Named ref into svc.Types and the renderer would point the
// method at them directly instead of synthesising a wrapper.
//
// The response message comes from acquireDynamicMessage; gat releases
// it through the route's Release once encoded.
func newCanonicalArgsGRPCHandler(disp ir.Dispatcher, outputDesc protoreflect.MessageDescriptor) Handler {
	wrapInValue := isSynthResponseWrapper(outputDesc)
	return Handler(func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		var args map[string]any
		if reqMsg, ok := req.(*dynamicpb.Message); ok && reqMsg != nil {
			args = messageToMap(reqMsg)
		} else {
			args = map[string]any{}
		}
		result, err := disp.Dispatch(ctx, args)
		if err != nil {
			return nil, err
		}
		respMsg := acquireDynamicMessage(outputDesc)
		if result == nil {
			return respMsg, nil
		}
		var payload map[string]any
		if wrapInValue {
			payload = map[string]any{"value": coerceCanonicalListValue(result)}
		} else if m, ok := result.(map[string]any); ok {
			payload = m
		} else {
			// Non-wrapper outputDesc but the dispatcher returned
			// something that isn't a map — refuse to lose the body.
			// argsToMessage doesn't carry a primitive-only path.
			releaseDynamicMessage(outputDesc, respMsg)
			return nil, fmt.Errorf("ingress: cannot project %T into %s", result, outputDesc.FullName())
		}
		if err := argsToMessage(payload, respMsg); err != nil {
			releaseDynamicMessage(outputDesc, respMsg)
			return nil, err
		}
		return respMsg, nil
	})
}

// isSynthResponseWrapper reports whether md has the structural
// shape ir.synthResponseMessage emits: exactly one field named
// "value". Used by the canonical-args gRPC handler to decide
// whether to wrap a dispatcher's return value.
func isSynthResponseWrapper(md protoreflect.MessageDescriptor) bool {
	if md == nil || md.Fields().Len() != 1 {
		return false
	}
	f := md.Fields().Get(0)
	return string(f.Name()) == "value"
}

// coerceCanonicalListValue normalises a dispatcher's return value
// to the JSON-decoded shape argsToMessage expects: lists must be
// []any. Both openapi and graphql dispatchers go through
// json.Unmarshal into `any` so the common case is already []any /
// map[string]any / scalar — but []map[string]any (e.g. a hand-built
// response in tests) shows up too. Unknown slice element types fall
// through unchanged so argsToMessage's per-field type check
// surfaces the mismatch.
func coerceCanonicalListValue(v any) any {
	if items, ok := v.([]map[string]any); ok {
		out := make([]any, len(items))
		for i, m := range items {
			out[i] = m
		}
		return out
	}
	return v
}

// GRPCUnknownHandler returns a grpc.StreamHandler the operator wires
// onto a *grpc.Server via grpc.UnknownServiceHandler — typically the
// same server that hosts ControlPlane:
//
//	srv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
//	cpv1.RegisterControlPlaneServer(srv, gw.ControlPlane())
//
// Unary RPCs decode wire bytes into a dynamicpb.Message of the target
// method's input descriptor, apply per-pool backpressure, and run the
// chained Handler the canonical protoDispatcher uses (so InjectType /
// HideType / user runtime middleware all apply). Skipping canonical
// args avoids two map↔message conversions on the proto→proto path.
//
// Server-streaming RPCs route through the canonical-args
// protoDirectSubscriptionDispatcher: args are converted to a
// dynamicpb request message and sent to the upstream via a direct
// gRPC server-streaming call. Each delivered event re-encodes to
// dynamicpb on the wire.
//
// Bidi / client-streaming RPCs return Unimplemented. Unmatched
// methods return Unimplemented.
//
// Safe to call before any service is registered; the handler reads
// the route table at dispatch time so registrations after the
// listener is up are visible immediately.
//
// The routes are served by gat's gRPC ingress, which ConnectHandler
// serves over Connect and gRPC-Web too.
//
// Stability: stable
func (g *Gateway) GRPCUnknownHandler() grpc.StreamHandler {
	if err := g.ensureAssembled(); err != nil {
		return func(any, grpc.ServerStream) error {
			return status.Errorf(codes.Internal, "ingress: assemble: %v", err)
		}
	}
	return g.gql.GRPCStreamHandler()
}

// ConnectHandler serves the routes GRPCUnknownHandler serves over
// Connect, gRPC-Web and gRPC (connect-go's protocols) on a plain
// http.Handler, at /package.Service/Method. Mount it where those paths
// arrive (http.StripPrefix for a prefix); native gRPC through it needs
// HTTP/2 (h2c or TLS). An unknown procedure answers 404.
//
// Stability: experimental
func (g *Gateway) ConnectHandler() http.Handler {
	if err := g.ensureAssembled(); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "ingress: assemble: "+err.Error(), http.StatusInternalServerError)
		})
	}
	return g.gql.GRPCHandler()
}

// ensureAssembled builds the first schema (and with it the gRPC
// routes) if no registration has yet.
func (g *Gateway) ensureAssembled() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.schema.Load() == nil {
		return g.assembleLocked()
	}
	return nil
}

// grpcPreCall wraps every gRPC ingress call (gat's PreCall hook):
// Unavailable while draining, then the ingress span and inject cache;
// for unary calls also the dispatch accumulator and the pool's
// backpressure slot. grpcUnaryDone / grpcStreamDone finish the call.
func (g *Gateway) grpcPreCall(ctx context.Context, call gat.GRPCCall) (context.Context, func(context.Context), error) {
	if g.draining.Load() {
		return ctx, nil, status.Error(codes.Unavailable, "ingress: draining")
	}
	info, _ := call.Data.(*grpcRouteInfo)
	if info == nil {
		info = noRouteInfo
	}
	ctx = g.tracer.extractGRPC(ctx)
	if call.Streaming {
		// Subscription lifetime is open-ended; skip request_*_seconds.
		// The span puts the ingress timing in the trace; child spans
		// on the dispatcher path land under it.
		ctx, _ = g.tracer.startIngressSpan(ctx, "gateway.grpc.subscription",
			ingressAttr("grpc"),
			namespaceAttr(info.namespace),
			versionAttr(info.version),
			methodAttr(call.Procedure),
			grpcSystemAttr(),
			rpcMethodAttr(call.Procedure),
		)
		ctx = withTracer(ctx, g.tracer)
		ctx = withInjectCache(ctx)
		return ctx, endIngressSpan, nil
	}
	ctx, _ = g.tracer.startIngressSpan(ctx, "gateway.grpc",
		ingressAttr("grpc"),
		namespaceAttr(info.namespace),
		versionAttr(info.version),
		methodAttr(call.Procedure),
		grpcSystemAttr(),
		rpcMethodAttr(call.Procedure),
	)
	ctx = withTracer(ctx, g.tracer)
	ctx, accum := withDispatchAccumulator(ctx)
	ctx = withInjectCache(ctx)
	accum.start = time.Now()
	accum.method = call.Procedure
	release, err := acquireBackpressureSlot(ctx, info.bp)
	if err != nil {
		return ctx, g.grpcDone, err
	}
	accum.release = release
	return ctx, g.grpcDone, nil
}

// grpcUnaryDone records a finished unary gRPC call from what
// grpcPreCall put on ctx, so one func (g.grpcDone) serves every call.
func (g *Gateway) grpcUnaryDone(ctx context.Context) {
	accum := dispatchAccumulatorFromContext(ctx)
	if accum.release != nil {
		accum.release()
	}
	total := time.Since(accum.start)
	dispatchSum := time.Duration(accum.Sum.Load())
	g.cfg.metrics.RecordRequest("grpc", total, total-dispatchSum)
	g.logRequestLine("grpc", accum.method, total, dispatchSum, int(accum.Count.Load()))
	oteltrace.SpanFromContext(ctx).End()
}

func endIngressSpan(ctx context.Context) {
	oteltrace.SpanFromContext(ctx).End()
}

// gRPC metadata keys the streaming ingress consults for subscription
// auth handles. Lowercase per gRPC convention; "x-gateway-" prefixed
// to avoid colliding with anything the upstream service might forward.
// Over Connect and gRPC-Web they are request headers.
const (
	mdSubscribeHMAC      = "x-gateway-hmac"
	mdSubscribeTimestamp = "x-gateway-timestamp"
	mdSubscribeKid       = "x-gateway-kid"
)

// grpcSubscription serves a server-streaming method through its
// canonical-args subscription dispatcher: the request becomes args
// (plus the HMAC handles from metadata), each event a response
// message.
func grpcSubscription(disp ir.Dispatcher, outputDesc protoreflect.MessageDescriptor) func(context.Context, *dynamicpb.Message, func(proto.Message) error) error {
	return func(ctx context.Context, req *dynamicpb.Message, send func(proto.Message) error) error {
		args := messageToMap(req)
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get(mdSubscribeHMAC); len(v) > 0 {
				args["hmac"] = v[0]
			}
			if v := md.Get(mdSubscribeTimestamp); len(v) > 0 {
				args["timestamp"] = v[0]
			}
			if v := md.Get(mdSubscribeKid); len(v) > 0 {
				args["kid"] = v[0]
			}
		}

		out, err := disp.Dispatch(ctx, args)
		if err != nil {
			return err
		}
		ch, ok := out.(chan any)
		if !ok {
			return status.Error(codes.Internal, "ingress: subscription dispatcher returned non-channel")
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ev, open := <-ch:
				if !open {
					return nil
				}
				evMap, ok := ev.(map[string]any)
				if !ok {
					continue
				}
				outMsg := acquireDynamicMessage(outputDesc)
				if err := argsToMessage(evMap, outMsg); err != nil {
					releaseDynamicMessage(outputDesc, outMsg)
					continue
				}
				err := send(outMsg)
				releaseDynamicMessage(outputDesc, outMsg)
				if err != nil {
					return err
				}
			}
		}
	}
}

// acquireBackpressureSlot is the slot-acquisition prologue shared by
// gRPC ingress and the canonical-args backpressureMiddleware.
// Returns a release func (always non-nil when err == nil; defer it
// to release the slot) and a Reject when the wait budget expires.
//
// Safe to call with cfg.Sem == nil — returns a no-op release and no
// error. Two dispatch paths through one helper means they can't
// drift on backpressure semantics.
func acquireBackpressureSlot(ctx context.Context, cfg backpressureConfig) (release func(), err error) {
	if cfg.Sem == nil {
		return func() {}, nil
	}
	waitStart := time.Now()
	select {
	case cfg.Sem <- struct{}{}:
		cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, time.Since(waitStart))
	default:
		depth := int(cfg.Queueing.Add(1))
		setQueueDepthForCfg(cfg, depth)
		dwell, werr := waitForSlot(ctx, cfg.Sem, cfg.MaxWaitTime)
		now := int(cfg.Queueing.Add(-1))
		setQueueDepthForCfg(cfg, now)
		cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, dwell)
		if werr != nil {
			cfg.Metrics.RecordBackoff(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, "wait_timeout")
			return nil, Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", cfg.Namespace, cfg.Version, werr.Error()))
		}
	}
	return func() { <-cfg.Sem }, nil
}
