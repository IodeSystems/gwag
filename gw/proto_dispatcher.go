package gateway

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// protoDispatcher implements ir.Dispatcher for one proto-pool RPC.
// It owns the marshal (canonical args → dynamicpb request) /
// unmarshal (dynamicpb response → map) bridge and runs the pool's
// pre-built proto-shaped Handler chain (user runtime middleware +
// inner pickReplica/Invoke). BackpressureMiddleware wraps the
// outside; the backpressure prologue is no longer inline here.
//
// Build once per (pool, RPC) at schema build time and reuse for
// every dispatch — the inner Handler chain is captured once.
type protoDispatcher struct {
	inputDesc protoreflect.MessageDescriptor
	handler   Handler // chain(inner): user runtime middleware around pickReplica+Invoke+RecordDispatch
	namespace string
	version   string
	op        string
}

// newProtoInvocationHandler returns the proto-native inner Handler
// for one (pool, RPC): pickReplica, increment inflight, gRPC Invoke,
// RecordDispatch. Shared by the canonical-args protoDispatcher and
// the proto-native gRPC ingress path — both want middleware wrapped
// around the same inner.
//
// The returned Handler is uncomposed: callers wrap it with the
// runtime middleware chain themselves. Backpressure is the outer
// layer in both call sites.
//
// Response message comes from the per-descriptor pool. The caller
// is responsible for calling releaseDynamicMessage on it once
// they've read out the contents (e.g. messageToMap) or written them
// to the wire (stream.SendMsg). On Invoke error the handler returns
// the message to the pool itself — `nil, err` doesn't dangle a
// pooled allocation.
func newProtoInvocationHandler(p *pool, sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor, metrics Metrics, bpOpts BackpressureOptions) Handler {
	method := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
	outputDesc := md.Output()
	ns, ver := p.key.namespace, p.key.version
	return Handler(func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		start := time.Now()
		r := p.pickReplica()
		if r == nil {
			err := fmt.Errorf("gateway: no live replicas for %s/%s", ns, ver)
			metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
			return nil, err
		}
		// Per-instance cap: acquired AFTER pickReplica because the
		// per-instance sem is keyed on a specific replica. Service-
		// level backpressure already gated entry above (in
		// BackpressureMiddleware on the canonical-args path; in the
		// gRPC ingress prologue otherwise).
		releaseInstance, err := acquireReplicaSlot(ctx, r, p, method, metrics, bpOpts)
		if err != nil {
			metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
			return nil, err
		}
		defer releaseInstance()

		r.inflight.Add(1)
		defer r.inflight.Add(-1)
		resp := acquireDynamicMessage(outputDesc)
		err = r.conn.Invoke(ctx, method, req, resp)
		metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
		if err != nil {
			releaseDynamicMessage(outputDesc, resp)
			return nil, err
		}
		return resp, nil
	})
}

// newProtoDispatcher wraps newProtoInvocationHandler with the user
// runtime middleware chain and adapts to the canonical-args
// ir.Dispatcher interface used by GraphQL / HTTP/JSON ingress.
//
// RecordDispatch fires from the inner Handler with the per-Invoke
// duration. Pre-cutover the same metric covered queue+invoke wall
// time; with BackpressureMiddleware now the outer layer the dwell
// metric carries the queue portion separately. No test asserts on
// the prior shape.
func newProtoDispatcher(p *pool, sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor, chain Middleware, metrics Metrics, bpOpts BackpressureOptions) *protoDispatcher {
	inner := newProtoInvocationHandler(p, sd, md, metrics, bpOpts)
	return &protoDispatcher{
		inputDesc: md.Input(),
		handler:   chain(inner),
		namespace: p.key.namespace,
		version:   p.key.version,
		op:        string(md.Name()),
	}
}

// Dispatch satisfies ir.Dispatcher. Marshals canonical args into a
// dynamicpb request, runs the captured Handler chain, unmarshals
// the response back to a canonical map.
//
// Both request and response messages come from
// acquireDynamicMessage; they're released after the response has
// been turned into the canonical map (gRPC has already finished
// marshaling by then, so the buffer is safe to reuse).
func (d *protoDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	ctx = withDispatchOpInfo(ctx, d.namespace, d.version, d.op)
	req := acquireDynamicMessage(d.inputDesc)
	defer releaseDynamicMessage(d.inputDesc, req)
	if err := argsToMessage(args, req); err != nil {
		return nil, err
	}
	resp, err := d.handler(ctx, req)
	if err != nil {
		return nil, err
	}
	respMsg := resp.(*dynamicpb.Message)
	out := messageToMap(respMsg)
	releaseDynamicMessage(respMsg.Descriptor(), respMsg)
	return out, nil
}

// methodLabel returns the proto wire path for an RPC, used as the
// metric label and the BackpressureMiddleware Label slot. Mirrors
// what the inner Handler computes for RecordDispatch so the two
// metrics share their per-method label space.
func methodLabel(sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor) string {
	return fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
}

// poolBackpressureConfig bundles a pool's per-dispatch knobs into a
// BackpressureConfig once per (pool, RPC). Callers pass it to
// BackpressureMiddleware to wrap the protoDispatcher.
func poolBackpressureConfig(p *pool, label string, metrics Metrics, bp BackpressureOptions) BackpressureConfig {
	return BackpressureConfig{
		Sem:         p.sem,
		Queueing:    &p.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   p.key.namespace,
		Version:     p.key.version,
		Label:       label,
		Kind:        "unary",
	}
}

// acquireReplicaSlot is the per-instance counterpart to
// acquireBackpressureSlot — acquires the replica's own sem, with the
// same MaxWaitTime budget and queue-depth tracking. The pool's
// MaxConcurrencyPerInstance setting drives the sem size; nil sem
// (unbounded per replica) returns a no-op release.
//
// Metrics use a separate "unary_instance" Kind so per-replica queue
// depth doesn't blur into the service-level "unary" counter.
func acquireReplicaSlot(ctx context.Context, r *replica, p *pool, label string, metrics Metrics, bp BackpressureOptions) (release func(), err error) {
	if r.sem == nil {
		return func() {}, nil
	}
	cfg := BackpressureConfig{
		Sem:         r.sem,
		Queueing:    &r.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   p.key.namespace,
		Version:     p.key.version,
		Label:       label,
		Kind:        "unary_instance",
	}
	return acquireBackpressureSlot(ctx, cfg)
}

// Compile-time assertion: protoDispatcher implements ir.Dispatcher.
var _ ir.Dispatcher = (*protoDispatcher)(nil)
