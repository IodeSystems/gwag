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
func newProtoInvocationHandler(p *pool, sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor, metrics Metrics) Handler {
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
		r.inflight.Add(1)
		defer r.inflight.Add(-1)
		resp := dynamicpb.NewMessage(outputDesc)
		err := r.conn.Invoke(ctx, method, req, resp)
		metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
		if err != nil {
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
func newProtoDispatcher(p *pool, sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor, chain Middleware, metrics Metrics) *protoDispatcher {
	inner := newProtoInvocationHandler(p, sd, md, metrics)
	return &protoDispatcher{
		inputDesc: md.Input(),
		handler:   chain(inner),
	}
}

// Dispatch satisfies ir.Dispatcher. Marshals canonical args into a
// dynamicpb request, runs the captured Handler chain, unmarshals
// the response back to a canonical map.
func (d *protoDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	req := dynamicpb.NewMessage(d.inputDesc)
	if err := argsToMessage(args, req); err != nil {
		return nil, err
	}
	resp, err := d.handler(ctx, req)
	if err != nil {
		return nil, err
	}
	return messageToMap(resp.(*dynamicpb.Message)), nil
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

// Compile-time assertion: protoDispatcher implements ir.Dispatcher.
var _ ir.Dispatcher = (*protoDispatcher)(nil)
