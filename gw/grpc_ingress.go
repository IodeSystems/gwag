package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// grpcIngressRoute is one (METHOD URL) → handler entry. Distinct
// from ingressRoute (HTTP/JSON) because gRPC ingress skips the
// canonical-args round trip on unary calls — wire bytes deserialize
// into a dynamicpb.Message of the input descriptor and feed straight
// into the chained Handler the canonical protoDispatcher uses.
//
// Streaming routes go through the canonical-args ir.Dispatcher
// (protoSubscriptionDispatcher) since subscribeNATS already keys off
// args; the input message converts to args once via messageToMap,
// metadata headers add hmac/timestamp/kid, and each delivered event
// gets re-encoded to dynamicpb on the wire.
//
// Built once per assembleLocked from the live pool set and discarded
// on rebuild. Atomic-swap so the unknown-service handler doesn't
// take g.mu on the hot path.
type grpcIngressRoute struct {
	pool       *pool
	inputDesc  protoreflect.MessageDescriptor
	outputDesc protoreflect.MessageDescriptor
	streaming  bool

	// Unary path: chain(inner) with the matching backpressure config.
	handler Handler
	bp      BackpressureConfig

	// Streaming path: ir.Dispatcher whose Dispatch returns a chan any
	// of decoded event maps from subscribeNATS.
	streamDispatcher ir.Dispatcher
}

type grpcIngressTable struct {
	routes map[string]*grpcIngressRoute // key: "/<svcFullName>/<methodName>"
}

// rebuildGRPCIngressLocked walks every proto pool's RPCs and emits
// one route per method. Unary routes capture the same chained
// Handler the canonical protoDispatcher uses (so transforms apply
// identically) alongside the per-pool BackpressureConfig. Server-
// streaming routes capture the protoSubscriptionDispatcher already
// registered in g.dispatchers — subscribeNATS handles HMAC + stream-
// slot acquisition + broker fanout.
//
// Bidi / client-streaming methods are skipped — egress doesn't
// support them and there's no canonical args shape to forward.
// Internal namespaces (`_*` or AsInternal) skip just like the
// GraphQL surface skips them.
//
// Caller holds g.mu.
func (g *Gateway) rebuildGRPCIngressLocked() {
	chain := g.runtimeChain()
	metrics := g.cfg.metrics
	bpOpts := g.cfg.backpressure
	t := &grpcIngressTable{routes: map[string]*grpcIngressRoute{}}
	for _, p := range g.pools {
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
				path := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
				if md.IsStreamingServer() {
					sid := ir.MakeSchemaID(p.key.namespace, p.key.version, string(md.Name()))
					sd := g.dispatchers.Get(sid)
					if sd == nil {
						continue
					}
					t.routes[path] = &grpcIngressRoute{
						pool:             p,
						inputDesc:        md.Input(),
						outputDesc:       md.Output(),
						streaming:        true,
						streamDispatcher: sd,
					}
					continue
				}
				inner := newProtoInvocationHandler(p, sd, md, metrics)
				label := methodLabel(sd, md)
				t.routes[path] = &grpcIngressRoute{
					pool:       p,
					inputDesc:  md.Input(),
					outputDesc: md.Output(),
					handler:    chain(inner),
					bp:         poolBackpressureConfig(p, label, metrics, bpOpts),
				}
			}
		}
	}
	g.grpcIngressRoutes.Store(t)
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
// chained Handler the canonical protoDispatcher uses (so HideAndInject
// / Hides / user runtime middleware all apply). Skipping canonical
// args avoids two map↔message conversions on the proto→proto path.
//
// Server-streaming RPCs route through the canonical-args
// protoSubscriptionDispatcher: messageToMap on the input plus
// hmac/timestamp/kid pulled from gRPC metadata
// (`x-gateway-hmac`/`-timestamp`/`-kid`) feed subscribeNATS, which
// handles HMAC verify, stream-slot acquisition, and broker fanout.
// Each delivered event re-encodes to dynamicpb on the wire.
//
// Bidi / client-streaming RPCs return Unimplemented. Unmatched
// methods return Unimplemented.
//
// Safe to call before any service is registered; the handler reads
// the route table at dispatch time so registrations after the
// listener is up are visible immediately.
func (g *Gateway) GRPCUnknownHandler() grpc.StreamHandler {
	g.mu.Lock()
	if g.schema.Load() == nil {
		if err := g.assembleLocked(); err != nil {
			g.mu.Unlock()
			return func(any, grpc.ServerStream) error {
				return status.Errorf(codes.Internal, "ingress: assemble: %v", err)
			}
		}
	}
	g.mu.Unlock()
	return g.serveGRPCUnknown
}

func (g *Gateway) serveGRPCUnknown(_ any, stream grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "ingress: missing method on stream")
	}
	if g.draining.Load() {
		return status.Error(codes.Unavailable, "ingress: draining")
	}
	t := g.grpcIngressRoutes.Load()
	if t == nil {
		return status.Errorf(codes.Unimplemented, "ingress: no routes (gateway not assembled)")
	}
	route, ok := t.routes[method]
	if !ok {
		return status.Errorf(codes.Unimplemented, "ingress: no route for %s", method)
	}

	if route.streaming {
		return g.serveGRPCStreamingUnknown(stream, route)
	}

	req := acquireDynamicMessage(route.inputDesc)
	defer releaseDynamicMessage(route.inputDesc, req)
	if err := stream.RecvMsg(req); err != nil {
		// EOF / cancellation propagate as-is so client sees the right
		// status; everything else maps to InvalidArgument since the
		// request bytes failed to decode against the descriptor.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return status.Errorf(codes.InvalidArgument, "ingress: decode request: %v", err)
	}

	ctx := withInjectCache(stream.Context())

	release, err := acquireBackpressureSlot(ctx, route.bp)
	if err != nil {
		return ingressGRPCStatus(err)
	}
	defer release()

	resp, err := route.handler(ctx, req)
	if err != nil {
		return ingressGRPCStatus(err)
	}
	respMsg := resp.(*dynamicpb.Message)
	sendErr := stream.SendMsg(respMsg)
	releaseDynamicMessage(route.outputDesc, respMsg)
	return sendErr
}

// gRPC metadata keys the streaming ingress consults for subscription
// auth handles. Lowercase per gRPC convention; "x-gateway-" prefixed
// to avoid colliding with anything the upstream service might forward.
const (
	mdSubscribeHMAC      = "x-gateway-hmac"
	mdSubscribeTimestamp = "x-gateway-timestamp"
	mdSubscribeKid       = "x-gateway-kid"
)

func (g *Gateway) serveGRPCStreamingUnknown(stream grpc.ServerStream, route *grpcIngressRoute) error {
	req := acquireDynamicMessage(route.inputDesc)
	defer releaseDynamicMessage(route.inputDesc, req)
	if err := stream.RecvMsg(req); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return status.Errorf(codes.InvalidArgument, "ingress: decode request: %v", err)
	}

	args := messageToMap(req)
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
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

	ctx := withInjectCache(stream.Context())

	out, err := route.streamDispatcher.Dispatch(ctx, args)
	if err != nil {
		return ingressGRPCStatus(err)
	}
	ch, ok := out.(chan any)
	if !ok {
		return status.Error(codes.Internal, "ingress: subscription dispatcher returned non-channel")
	}

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case ev, open := <-ch:
			if !open {
				return nil
			}
			evMap, ok := ev.(map[string]any)
			if !ok {
				continue
			}
			outMsg := acquireDynamicMessage(route.outputDesc)
			if err := argsToMessage(evMap, outMsg); err != nil {
				releaseDynamicMessage(route.outputDesc, outMsg)
				continue
			}
			err := stream.SendMsg(outMsg)
			releaseDynamicMessage(route.outputDesc, outMsg)
			if err != nil {
				return err
			}
		}
	}
}

// acquireBackpressureSlot is the slot-acquisition prologue shared by
// gRPC ingress and the canonical-args BackpressureMiddleware.
// Returns a release func (always non-nil when err == nil; defer it
// to release the slot) and a Reject when the wait budget expires.
//
// Safe to call with cfg.Sem == nil — returns a no-op release and no
// error. Two dispatch paths through one helper means they can't
// drift on backpressure semantics.
func acquireBackpressureSlot(ctx context.Context, cfg BackpressureConfig) (release func(), err error) {
	if cfg.Sem == nil {
		return func() {}, nil
	}
	waitStart := time.Now()
	select {
	case cfg.Sem <- struct{}{}:
		cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, time.Since(waitStart))
	default:
		depth := int(cfg.Queueing.Add(1))
		cfg.Metrics.SetQueueDepth(cfg.Namespace, cfg.Version, cfg.Kind, depth)
		dwell, werr := waitForSlot(ctx, cfg.Sem, cfg.MaxWaitTime)
		now := int(cfg.Queueing.Add(-1))
		cfg.Metrics.SetQueueDepth(cfg.Namespace, cfg.Version, cfg.Kind, now)
		cfg.Metrics.RecordDwell(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, dwell)
		if werr != nil {
			cfg.Metrics.RecordBackoff(cfg.Namespace, cfg.Version, cfg.Label, cfg.Kind, "wait_timeout")
			return nil, Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", cfg.Namespace, cfg.Version, werr.Error()))
		}
	}
	return func() { <-cfg.Sem }, nil
}

// ingressGRPCStatus maps an error from the ingress dispatch path
// onto the gRPC status code clients expect. Reject errors carry
// the gateway Code enum; other errors fall through to Internal.
//
// gRPC status errors from the upstream Invoke are pass-through (the
// gateway doesn't reclassify upstream codes).
func ingressGRPCStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		// Upstream gRPC error already classified — pass through.
		return err
	}
	var rej *rejection
	if errors.As(err, &rej) {
		return status.Error(codeToGRPC(rej.Code), rej.Msg)
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// codeToGRPC is the inverse of httpStatusToCode / codeToHTTPStatus —
// classifies a gateway Code onto its gRPC status equivalent.
func codeToGRPC(c Code) codes.Code {
	switch c {
	case CodeUnauthenticated:
		return codes.Unauthenticated
	case CodePermissionDenied:
		return codes.PermissionDenied
	case CodeResourceExhausted:
		return codes.ResourceExhausted
	case CodeInvalidArgument:
		return codes.InvalidArgument
	case CodeNotFound:
		return codes.NotFound
	}
	return codes.Internal
}
