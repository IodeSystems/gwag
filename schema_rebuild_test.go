package gateway

import (
	"strings"
	"testing"

	aav1 "github.com/iodesystems/go-api-gateway/adminauth/v1"
	eav1 "github.com/iodesystems/go-api-gateway/eventsauth/v1"
	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// hasQueryField reports whether the gateway's current schema has the
// given top-level Query field. nil schema → false.
func hasQueryField(g *Gateway, name string) bool {
	s := g.schema.Load()
	if s == nil {
		return false
	}
	q := s.QueryType()
	if q == nil {
		return false
	}
	_, ok := q.Fields()[name]
	return ok
}

// newSchemaTestGateway returns a gateway with a schema already
// assembled (Handler() forces assemble) so subsequent registrations
// trigger the rebuild path rather than just deferring to Handler.
func newSchemaTestGateway(t *testing.T) *Gateway {
	t.Helper()
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("ignored")))
	t.Cleanup(gw.Close)
	// Force initial assemble so the next AddProto rebuilds in place.
	_ = gw.Handler()
	return gw
}

func TestSchemaRebuild_PoolCreateAddsField(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if hasQueryField(gw, "greeter") {
		t.Fatal("schema unexpectedly has greeter before registration")
	}
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	if !hasQueryField(gw, "greeter") {
		t.Fatal("schema missing greeter field after registration")
	}
}

func TestSchemaRebuild_PoolDestroyRemovesField(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	if !hasQueryField(gw, "greeter") {
		t.Fatal("schema missing greeter post-add")
	}

	// Boot-time AddProtoDescriptor stores owner="". Drain the pool by
	// owner; this removes the only replica, deletes the empty pool,
	// and triggers a schema rebuild.
	gw.mu.Lock()
	removed, err := gw.removeReplicasByOwnerLocked("")
	gw.mu.Unlock()
	if err != nil {
		t.Fatalf("removeReplicasByOwnerLocked: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d replicas, want 1", removed)
	}
	if hasQueryField(gw, "greeter") {
		t.Fatal("schema still has greeter after pool destroy")
	}
}

func TestSchemaRebuild_HashMismatchRejected(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("shared"),
	); err != nil {
		t.Fatalf("first AddProtoDescriptor: %v", err)
	}
	// Different proto under the same namespace must reject — protects
	// pools from accidental cross-service registration.
	err := gw.AddProtoDescriptor(
		eav1.File_eventsauth_proto,
		To(nopGRPCConn{}),
		As("shared"),
	)
	if err == nil {
		t.Fatal("expected hash-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "different proto hash") {
		t.Fatalf("error message: %v (want 'different proto hash')", err)
	}
}

func TestSchemaRebuild_SameHashJoinsPool(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("first AddProtoDescriptor: %v", err)
	}
	// Second registration with the same descriptor must be allowed —
	// represents two replicas serving the same proto.
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("second AddProtoDescriptor (same hash): %v", err)
	}
	gw.mu.Lock()
	p := gw.pools[poolKey{namespace: "greeter", version: "v1"}]
	gw.mu.Unlock()
	if p == nil {
		t.Fatal("greeter pool missing")
	}
	if got := p.replicaCount(); got != 2 {
		t.Fatalf("replica count = %d, want 2", got)
	}
}

func TestSchemaRebuild_MultipleNamespaces(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("greeter: %v", err)
	}
	if err := gw.AddProtoDescriptor(
		aav1.File_adminauth_v1_adminauth_proto,
		To(nopGRPCConn{}),
		As("authd"),
	); err != nil {
		t.Fatalf("authd: %v", err)
	}
	if !hasQueryField(gw, "greeter") {
		t.Fatal("greeter missing")
	}
	if !hasQueryField(gw, "authd") {
		t.Fatal("authd missing")
	}
}

func TestSchemaRebuild_UnderscoreNamespaceAutoInternal(t *testing.T) {
	// Reserved-namespace convention: anything starting with "_" is
	// auto-internal, even when AsInternal() isn't passed. Prevents
	// accidental leak of _admin_auth / _events_auth / _admin_events.
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("_secret_ns"), // no AsInternal()
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	if hasQueryField(gw, "_secret_ns") {
		t.Fatal("_-prefixed namespace leaked into Query (auto-internal failed)")
	}
	// Pool is still registered (dispatchable from hooks etc.).
	gw.mu.Lock()
	_, ok := gw.pools[poolKey{namespace: "_secret_ns", version: "v1"}]
	gw.mu.Unlock()
	if !ok {
		t.Fatal("_secret_ns pool missing from registry")
	}
}

func TestSchemaRebuild_AsInternalHidesFromQuery(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoDescriptor(
		greeterv1.File_greeter_proto,
		To(nopGRPCConn{}),
		As("hidden"),
		AsInternal(),
	); err != nil {
		t.Fatalf("AddProtoDescriptor: %v", err)
	}
	if hasQueryField(gw, "hidden") {
		t.Fatal("internal namespace leaked into Query")
	}
	// But the pool exists — internal services are dispatchable from
	// hooks even though they don't surface in the public schema.
	gw.mu.Lock()
	_, ok := gw.pools[poolKey{namespace: "hidden", version: "v1"}]
	gw.mu.Unlock()
	if !ok {
		t.Fatal("internal pool missing from registry")
	}
}
