package gateway

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/gw/ir"
)

// protoDirectSubscriptionDispatcher implements ir.Dispatcher for one
// server-streaming proto RPC using honest per-subscriber upstream
// dispatch. Each subscriber gets its own gRPC stream to a replica;
// frames from the upstream are forwarded as map[string]any events
// through a channel that graphql-go's executor pumps as WebSocket
// next-frames.
//
// Reuses the existing streamSem backpressure (pool.streamSem +
// gateway streamGlobalSem) via subscribeNATS's slot acquisition.
// BackpressureMiddleware is intentionally NOT wrapped around this
// dispatcher — same rationale as protoSubscriptionDispatcher.
type protoDirectSubscriptionDispatcher struct {
	g          *Gateway
	pool       *pool
	sd         protoreflect.ServiceDescriptor
	md         protoreflect.MethodDescriptor
	inputDesc  protoreflect.MessageDescriptor
	outputDesc protoreflect.MessageDescriptor
}

func newProtoDirectSubscriptionDispatcher(g *Gateway, p *pool, sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor) *protoDirectSubscriptionDispatcher {
	return &protoDirectSubscriptionDispatcher{
		g:          g,
		pool:       p,
		sd:         sd,
		md:         md,
		inputDesc:  md.Input(),
		outputDesc: md.Output(),
	}
}

// Dispatch opens a direct gRPC server-streaming call to an upstream
// replica and returns a chan any of decoded event maps. The channel
// is closed when the upstream stream ends or the context cancels.
// Stream slots (gateway-wide + per-pool) are acquired with the same
// backpressure semantics as the NATS broker path.
func (d *protoDirectSubscriptionDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	ns := d.pool.key.namespace
	ver := d.pool.key.version
	method := fmt.Sprintf("/%s/%s", d.sd.FullName(), d.md.Name())

	// Acquire stream slots: gateway-wide cap first, then per-pool.
	if d.g.streamGlobalSem != nil {
		waitStart := time.Now()
		select {
		case d.g.streamGlobalSem <- struct{}{}:
			d.g.cfg.metrics.RecordDwell(ns, ver, string(d.md.Name()), "stream_global", time.Since(waitStart))
		default:
			depth := int(d.g.streamGlobalQ.Add(1))
			d.g.cfg.metrics.SetQueueDepth("", "", "stream_global", depth)
			dwell, err := waitForSlot(ctx, d.g.streamGlobalSem, d.g.cfg.backpressure.MaxWaitTime)
			now := int(d.g.streamGlobalQ.Add(-1))
			d.g.cfg.metrics.SetQueueDepth("", "", "stream_global", now)
			d.g.cfg.metrics.RecordDwell(ns, ver, string(d.md.Name()), "stream_global", dwell)
			if err != nil {
				d.g.cfg.metrics.RecordBackoff(ns, ver, string(d.md.Name()), "stream_global", "wait_timeout")
				return nil, Reject(CodeResourceExhausted, fmt.Sprintf("gateway stream cap: %s", err.Error()))
			}
		}
	}
	if d.pool.streamSem != nil {
		waitStart := time.Now()
		select {
		case d.pool.streamSem <- struct{}{}:
			d.g.cfg.metrics.RecordDwell(ns, ver, string(d.md.Name()), "stream", time.Since(waitStart))
		default:
			depth := int(d.pool.streamQueueing.Add(1))
			d.g.cfg.metrics.SetQueueDepth(ns, ver, "stream", depth)
			dwell, err := waitForSlot(ctx, d.pool.streamSem, d.g.cfg.backpressure.MaxWaitTime)
			now := int(d.pool.streamQueueing.Add(-1))
			d.g.cfg.metrics.SetQueueDepth(ns, ver, "stream", now)
			d.g.cfg.metrics.RecordDwell(ns, ver, string(d.md.Name()), "stream", dwell)
			if err != nil {
				d.g.cfg.metrics.RecordBackoff(ns, ver, string(d.md.Name()), "stream", "wait_timeout")
				if d.g.streamGlobalSem != nil {
					<-d.g.streamGlobalSem
				}
				return nil, Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", ns, ver, err.Error()))
			}
		}
	}

	rollbackSlots := func() {
		if d.pool.streamSem != nil {
			<-d.pool.streamSem
		}
		if d.g.streamGlobalSem != nil {
			<-d.g.streamGlobalSem
		}
	}

	r := d.pool.pickReplica()
	if r == nil {
		rollbackSlots()
		return nil, fmt.Errorf("gateway: no live replicas for %s/%s", ns, ver)
	}

	// Build request message from args.
	req := acquireDynamicMessage(d.inputDesc)
	defer releaseDynamicMessage(d.inputDesc, req)
	if err := argsToMessage(args, req); err != nil {
		rollbackSlots()
		return nil, err
	}

	// Apply header injectors.
	headers := d.g.headerInjectorSnapshot()
	if len(headers) > 0 {
		injected, err := applyHeaderInjectors(ctx, headers)
		if err != nil {
			rollbackSlots()
			return nil, err
		}
		if len(injected) > 0 {
			kvs := make([]string, 0, 2*len(injected))
			for k, v := range injected {
				kvs = append(kvs, k, v)
			}
			ctx = metadata.AppendToOutgoingContext(ctx, kvs...)
		}
	}

	// Open server-streaming call.
	streamDesc := &grpc.StreamDesc{
		ServerStreams: true,
		ClientStreams: false,
	}
	stream, err := r.conn.NewStream(ctx, streamDesc, method)
	if err != nil {
		rollbackSlots()
		return nil, err
	}

	// Send request and close send side.
	if err := stream.SendMsg(req); err != nil {
		rollbackSlots()
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		rollbackSlots()
		return nil, err
	}

	// Track active streams.
	poolN := int(d.pool.streamInflight.Add(1))
	d.g.cfg.metrics.SetStreamsInflight(ns, ver, poolN)
	globalN := int(d.g.streamGlobal.Add(1))
	d.g.cfg.metrics.SetStreamsInflightTotal(globalN)

	// Pump upstream frames into a channel.
	ch := make(chan any, 32)
	go func() {
		<-ctx.Done()
		poolNow := int(d.pool.streamInflight.Add(-1))
		d.g.cfg.metrics.SetStreamsInflight(ns, ver, poolNow)
		globalNow := int(d.g.streamGlobal.Add(-1))
		d.g.cfg.metrics.SetStreamsInflightTotal(globalNow)
		rollbackSlots()
	}()
	go func() {
		defer close(ch)
		for {
			resp := acquireDynamicMessage(d.outputDesc)
			err := stream.RecvMsg(resp)
			if err != nil {
				releaseDynamicMessage(d.outputDesc, resp)
				return
			}
			payload := messageToMap(resp)
			releaseDynamicMessage(d.outputDesc, resp)
			select {
			case ch <- payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

var _ ir.Dispatcher = (*protoDirectSubscriptionDispatcher)(nil)
