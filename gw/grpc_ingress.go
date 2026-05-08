package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// grpcIngressRoute is one (METHOD URL) → proto-native handler entry.
// Distinct from ingressRoute (HTTP/JSON) because gRPC ingress skips
// the canonical-args round trip — wire bytes deserialize into a
// dynamicpb.Message of the input descriptor and feed straight into
// the chained Handler the canonical protoDispatcher uses.
//
// Built once per assembleLocked from the live pool set and discarded
// on rebuild. Atomic-swap so the unknown-service handler doesn't
// take g.mu on the hot path.
type grpcIngressRoute struct {
	pool       *pool
	inputDesc  protoreflect.MessageDescriptor
	outputDesc protoreflect.MessageDescriptor
	handler    Handler // chain(inner): user runtime middleware around pickReplica+Invoke+RecordDispatch
	bp         BackpressureConfig
}

type grpcIngressTable struct {
	routes map[string]*grpcIngressRoute // key: "/<svcFullName>/<methodName>"
}

// rebuildGRPCIngressLocked walks every proto pool's unary RPCs and
// emits one route per method, capturing the same chained Handler the
// canonical protoDispatcher uses (so transforms apply identically)
// alongside the per-pool BackpressureConfig.
//
// Internal namespaces and streaming methods are skipped — the
// subscription path is graphql-ws today; HTTP/SSE / gRPC server-
// streaming follow in the subscription-transport-agnosticism
// workstream.
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
				if md.IsStreamingClient() || md.IsStreamingServer() {
					continue
				}
				inner := newProtoInvocationHandler(p, sd, md, metrics)
				label := methodLabel(sd, md)
				t.routes[fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())] = &grpcIngressRoute{
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
// The handler decodes wire bytes into a dynamicpb.Message of the
// target method's input descriptor, applies per-pool backpressure,
// runs the same chained Handler the canonical protoDispatcher uses
// (so HideAndInject / Hides / user runtime middleware all apply),
// and writes the dynamicpb response back. Skipping canonical args
// avoids two map↔message conversions on the proto→proto hot path.
//
// Unary only. Server-streaming RPCs aren't routed here — clients
// connect through the subscription transport instead. Unmatched
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

	req := dynamicpb.NewMessage(route.inputDesc)
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
	return stream.SendMsg(resp)
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
