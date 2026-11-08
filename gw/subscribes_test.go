package gateway

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Subscription edges: ServiceBinding.subscribes counts like a depends
// edge on the line that declares the channel.

// ordersEventsBinding is a control-plane binding at orders/<ver>
// whose proto (package orders.<ver>) declares the channel
// orders.<ver>.OrderEvents on events.orders.<ver>.{orderId}.
func ordersEventsBinding(t *testing.T, ver string) *cpv1.ServiceBinding {
	t.Helper()
	opts, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	src := `syntax = "proto3";
package orders.` + ver + `;
import "gw/proto/ps/v1/options.proto";
message OrderCreated {
  option (gwag.ps.v1.binding) = { name: "OrderEvents", pattern: "events.orders.` + ver + `.{orderId}" };
  string order_id = 1;
}
service Orders { rpc Get(OrderCreated) returns (OrderCreated); }
`
	return &cpv1.ServiceBinding{
		Namespace:    "orders",
		Version:      ver,
		ProtoSource:  []byte(src),
		ProtoImports: map[string][]byte{"gw/proto/ps/v1/options.proto": opts},
	}
}

func subscribesTo(b *cpv1.ServiceBinding, channel, group string) *cpv1.ServiceBinding {
	b.Subscribes = append(b.Subscribes, &cpv1.Subscription{Channel: channel, Group: group})
	return b
}

func TestSubscribes_GateWaitsForDeclarer(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutBackpressure())
	t.Cleanup(g.Close)

	gr := mustRegister(t, g, addr, subscribesTo(greeterBinding(t, "v1"), "orders.v1.OrderEvents", "greeter"))
	wantStates(t, gr.GetBindings(), stWaiting)
	d := gr.GetBindings()[0].GetDepends()
	if len(d) != 1 || d[0].GetChannel() != "orders.v1.OrderEvents" || d[0].GetGroup() != "greeter" || d[0].GetService() != "" {
		t.Fatalf("depends=%v, want undeclared channel", d)
	}
	wantMetric(t, g, 1, "dependency_unmet", "consumer_namespace", "greeter", "provider", "", "channel", "orders.v1.OrderEvents")

	mustRegister(t, g, addr, ordersEventsBinding(t, "v1"))
	st := heartbeatStates(t, g, gr.GetRegistrationId())
	wantStates(t, st, stReady)
	d = st[0].GetDepends()
	if len(d) != 1 || d[0].GetService() != "orders" || !reflect.DeepEqual(d[0].GetLines(), []string{"v1"}) ||
		!reflect.DeepEqual(d[0].GetPresentLines(), []string{"v1"}) || d[0].GetChannel() != "orders.v1.OrderEvents" {
		t.Fatalf("depends=%v, want orders/v1 present", d)
	}
	wantMetric(t, g, 0, "dependency_unmet", "consumer_namespace", "greeter", "provider", "orders", "channel", "orders.v1.OrderEvents")
	_, info := listServiceInfo(t, g, "greeter", "v1")
	if s := info.GetSubscribes(); len(s) != 1 || s[0].GetChannel() != "orders.v1.OrderEvents" || s[0].GetGroup() != "greeter" {
		t.Errorf("listed subscribes=%v", s)
	}
}

// A binding that subscribes to a channel its own proto declares does
// not wait for itself.
func TestSubscribes_OwnChannelDoesNotWait(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	r := mustRegister(t, g, addr, subscribesTo(ordersEventsBinding(t, "v1"), "orders.v1.OrderEvents", "outbox"))
	wantStates(t, r.GetBindings(), stReady)
}

func TestSubscribes_RejectsBadEntries(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	for _, b := range []*cpv1.ServiceBinding{
		subscribesTo(greeterBinding(t, "v1"), "", "g"),
		subscribesTo(greeterBinding(t, "v1"), "nodot", "g"),
		subscribesTo(subscribesTo(greeterBinding(t, "v1"), "a.v1.E", "g"), "a.v1.E", "g"),
	} {
		_, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{b}})
		if err == nil || !strings.Contains(err.Error(), "subscribes") {
			t.Errorf("subscribes=%v: err=%v", b.GetSubscribes(), err)
		}
	}
}

// The dependency graph shows the channel edge, and a subscriber keeps
// the declaring line from being sunset-able.
func TestSubscribes_GraphEdgeBlocksSunset(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	t.Cleanup(g.Close)
	mustRegister(t, g, addr, ordersEventsBinding(t, "v1"))
	mustRegister(t, g, addr, ordersEventsBinding(t, "v2"))

	// Two lines, no dependents, no traffic: v1 is sunset-able.
	if n := getGraph(t, g, "").node(t, "orders/v1"); !n.Sunsetable {
		t.Fatalf("orders/v1 before subscriber: %+v", n)
	}

	gr := mustRegister(t, g, addr, subscribesTo(greeterBinding(t, "v1"), "orders.v1.OrderEvents", "greeter"))
	wantStates(t, gr.GetBindings(), stReady)

	reg, err := g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.buildDependencyGraph(reg, "1h", time.Hour, adminView{full: true})
	if err != nil {
		t.Fatal(err)
	}
	var edge *graphDesiredEdge
	for i, e := range out.Body.DesiredEdges {
		if e.ConsumerNamespace == "greeter" {
			edge = &out.Body.DesiredEdges[i]
		}
	}
	if edge == nil || edge.EdgeKind != "channel" || edge.Channel != "orders.v1.OrderEvents" || edge.Group != "greeter" ||
		edge.Provider != "orders" || !reflect.DeepEqual(edge.Lines, []string{"v1"}) || !edge.Met || edge.InstanceCount != 1 {
		t.Fatalf("channel edge=%+v", edge)
	}
	for _, n := range out.Body.Nodes {
		if n.ID == "orders/v1" && (n.Sunsetable || !strings.Contains(n.SunsetReason, "subscribes to orders.v1.OrderEvents")) {
			t.Errorf("orders/v1 with subscriber: sunsetable=%v reason=%q", n.Sunsetable, n.SunsetReason)
		}
		if n.ID == "orders/v2" && !n.Sunsetable {
			t.Errorf("orders/v2 has no subscriber, should stay sunset-able: %q", n.SunsetReason)
		}
	}
}

// Cluster: the subscriber waits on node A, the declaring service
// registers on node B, A's gate opens, and both nodes list the
// subscription and the channel edge.
func TestSubscribes_ClusterCrossNode(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = testCluster.a.JS.DeleteStream(ctx, "EV_orders_v1_OrderEvents")
	})
	_, addr := startGreeterServer(t)
	register := func(n *clusterNode, bs ...*cpv1.ServiceBinding) *cpv1.RegisterResponse {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var lastErr error
		for ctx.Err() == nil {
			resp, err := n.gw.ControlPlane().Register(ctx, &cpv1.RegisterRequest{Addr: addr, Services: bs})
			if err == nil {
				return resp
			}
			lastErr = err
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("Register: %v", lastErr)
		return nil
	}

	gr := register(a, subscribesTo(greeterBinding(t, "v1"), "orders.v1.OrderEvents", "greeter"))
	wantStates(t, gr.GetBindings(), stWaiting)
	register(b, ordersEventsBinding(t, "v1"))
	waitGate(t, 30*time.Second, "greeter READY on A", func() bool {
		st := heartbeatStates(t, a.gw, gr.GetRegistrationId())
		return len(st) == 1 && st[0].GetState() == stReady
	})
	waitForPool(t, b.gw, "greeter", "v1", 30*time.Second)
	for _, n := range []*clusterNode{a, b} {
		waitGate(t, 15*time.Second, "subscription edge listed", func() bool {
			_, info := listServiceInfo(t, n.gw, "greeter", "v1")
			if info == nil || len(info.GetSubscribes()) != 1 || len(info.GetInstances()) != 1 {
				return false
			}
			d := info.GetInstances()[0].GetDepends()
			return len(d) == 1 && d[0].GetChannel() == "orders.v1.OrderEvents" && d[0].GetService() == "orders" && len(d[0].GetPresentLines()) == 1
		})
	}
}
