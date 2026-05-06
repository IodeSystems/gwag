package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	aev1 "github.com/iodesystems/go-api-gateway/adminevents/v1"
)

// adminEventsNamespace is the registration namespace under which the
// AdminEvents proto is exposed. Surfaces in the SDL as
// `admin_events_watchServices`.
const adminEventsNamespace = "admin_events"

// Shortcuts so the registry hooks don't have to import the
// adminevents package directly.
const (
	adminEventsActionRegistered   = aev1.ServiceChange_ACTION_REGISTERED
	adminEventsActionDeregistered = aev1.ServiceChange_ACTION_DEREGISTERED
)

// AddAdminEvents registers the built-in AdminEvents proto so the
// gateway's GraphQL schema gains the `admin_events_watchServices`
// Subscription field. Whenever the registry mutates, the gateway
// publishes a ServiceChange to NATS — clients subscribed via WS
// receive frames in real time.
//
// Cluster mode is required (subscriptions are NATS-backed); calling
// this without a configured cluster returns an error.
func (g *Gateway) AddAdminEvents() error {
	if g.cfg.cluster == nil {
		return errors.New("gateway: AddAdminEvents requires a configured cluster (NATS-backed subscriptions)")
	}
	return g.AddProtoDescriptor(
		aev1.File_adminevents_v1_adminevents_proto,
		To(noopAdminEventsConn{}),
		As(adminEventsNamespace),
	)
}

// noopAdminEventsConn satisfies grpc.ClientConnInterface so
// AddProtoDescriptor accepts it. The gateway never dispatches the
// AdminEvents methods through gRPC — the streaming method is
// NATS-backed and the request type has no unary callers.
type noopAdminEventsConn struct{}

func (noopAdminEventsConn) Invoke(_ context.Context, method string, _, _ any, _ ...grpc.CallOption) error {
	return fmt.Errorf("admin_events: gRPC dispatch not supported on %s (NATS-backed)", method)
}

func (noopAdminEventsConn) NewStream(_ context.Context, _ *grpc.StreamDesc, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("admin_events: gRPC streaming dispatch not supported on %s (NATS-backed)", method)
}

// publishServiceChange fires a ServiceChange event onto the NATS
// subject the admin_events_watchServices Subscription field reads
// from. No-op when the gateway isn't in cluster mode.
//
// Subject convention mirrors gateway-side subjectFor:
//
//	events.admin_events.WatchServices.<namespace>
//
// so subscribers with `namespace=""` get a NATS wildcard match
// (`events.admin_events.WatchServices.*`).
func (g *Gateway) publishServiceChange(action aev1.ServiceChange_Action, ns, ver, addr string, replicas uint32) {
	cl := g.cfg.cluster
	if cl == nil || cl.Conn == nil {
		return
	}
	msg := &aev1.ServiceChange{
		Action:          action,
		Namespace:       ns,
		Version:         ver,
		Addr:            addr,
		TimestampUnixMs: time.Now().UnixMilli(),
		ReplicaCount:    replicas,
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return
	}
	subject := fmt.Sprintf("events.%s.WatchServices.%s", adminEventsNamespace, ns)
	_ = cl.Conn.Publish(subject, payload)
}
