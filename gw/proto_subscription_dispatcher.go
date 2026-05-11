package gateway

import (
	"context"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/gw/ir"
)

// protoSubscriptionDispatcher implements ir.Dispatcher for one
// server-streaming proto RPC. Dispatch returns a chan any of decoded
// events from the gateway's NATS subscription broker — graphql-go's
// Subscribe field receives that chan, the renderer's Resolve plucks
// each frame back out as rp.Source.
//
// BackpressureMiddleware is intentionally NOT wrapped around this
// dispatcher: pool.sem is per-pool unary capacity, not stream
// lifetime. Stream slots are gated separately inside subscribeNATS
// (pool.streamSem + g.streamGlobalSem) with their own dwell / queue /
// backoff metrics, mirroring the pre-cutover legacy buildSubscriptionField.
type protoSubscriptionDispatcher struct {
	g          *Gateway
	ns         string
	ver        string
	methodName string
	outputDesc protoreflect.MessageDescriptor
}

func newProtoSubscriptionDispatcher(g *Gateway, ns, ver, methodName string, outputDesc protoreflect.MessageDescriptor) *protoSubscriptionDispatcher {
	return &protoSubscriptionDispatcher{
		g:          g,
		ns:         ns,
		ver:        ver,
		methodName: methodName,
		outputDesc: outputDesc,
	}
}

// Dispatch returns the NATS-backed event channel. The returned `any`
// is `chan any`; graphql-go's executor pumps frames off it and feeds
// each as rp.Source to the field's Resolve closure.
func (d *protoSubscriptionDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	return d.g.subscribeNATS(ctx, d.ns, d.ver, d.methodName, args, d.outputDesc)
}

var _ ir.Dispatcher = (*protoSubscriptionDispatcher)(nil)
