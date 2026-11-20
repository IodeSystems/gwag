package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// A registration whose schema cannot build is refused; the services
// already registered keep serving. The clash used here is real:
// col.v1.Thing and col.V1_Thing both render as the GraphQL type
// Col_V1_Thing, a duplicate type name graphql.NewSchema rejects.

const (
	checkGoodProto = `syntax = "proto3"; package col.v1;
message Thing { string a = 1; }
service S { rpc Get(Thing) returns (Thing); }`
	checkBadProto = `syntax = "proto3"; package col;
message V1_Thing { string b = 1; }
service T { rpc Put(V1_Thing) returns (V1_Thing); }`
	checkOtherProto = `syntax = "proto3"; package other.v1;
message Thing { string a = 1; }
service S { rpc Get(Thing) returns (Thing); }`
)

func checkQuery(t *testing.T, gw *Gateway, q string) string {
	t.Helper()
	_, body := compatPost(t, gw.Handler(), q, "")
	return strings.TrimSpace(body)
}

func TestSchemaCheck_LibraryRefusesBadRegistration(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "boot"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			gw := metricsGateway(t)
			if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
				t.Fatal(err)
			}
			if live {
				gw.Handler()
			}
			err := gw.AddProtoBytes("colb.proto", []byte(checkBadProto), To(checkConn{}), As("colb"))
			if err == nil || !strings.Contains(err.Error(), "schema does not build") || !strings.Contains(err.Error(), "colb/v1") {
				t.Fatalf("bad registration: err = %v, want a schema build refusal naming colb/v1", err)
			}
			gw.mu.Lock()
			_, has := gw.slots[poolKey{namespace: "colb", version: "v1"}]
			gw.mu.Unlock()
			if has {
				t.Fatal("refused registration left a slot")
			}
			// Not poisoned: a later good registration still builds.
			if err := gw.AddProtoBytes("other.proto", []byte(checkOtherProto), To(checkConn{}), As("other")); err != nil {
				t.Fatalf("good registration after a refused one: %v", err)
			}
			if got, want := checkQuery(t, gw, `{ col { get { a } } other { get { a } } }`), `{"data":{"col":{"get":{"a":""}},"other":{"get":{"a":""}}}}`; got != want {
				t.Fatalf("query: %s, want %s", got, want)
			}
			body := scrapeBody(t, gw)
			wantSeries(t, body, mp+`registrations_total{kind="proto",outcome="rejected"}`, "1")
		})
	}
}

// An unstable swap that cannot build keeps the previous occupant.
func TestSchemaCheck_UnstableSwapKeepsOccupant(t *testing.T) {
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddProtoBytes("other.proto", []byte(checkOtherProto), To(checkConn{}), As("u"), Version("unstable")); err != nil {
		t.Fatal(err)
	}
	gw.Handler()
	if err := gw.AddProtoBytes("colb.proto", []byte(checkBadProto), To(checkConn{}), As("u"), Version("unstable")); err == nil {
		t.Fatal("swap to a schema that does not build was accepted")
	}
	// The next rebuild still has the old occupant.
	if err := gw.AddProtoBytes("other.proto", []byte(strings.ReplaceAll(checkOtherProto, "other.v1", "more.v1")), To(checkConn{}), As("more")); err != nil {
		t.Fatalf("good registration after a refused swap: %v", err)
	}
	if got, want := checkQuery(t, gw, `{ col { get { a } } u { get { a } } more { get { a } } }`), `{"data":{"col":{"get":{"a":""}},"u":{"get":{"a":""}},"more":{"get":{"a":""}}}}`; got != want {
		t.Fatalf("query: %s, want %s", got, want)
	}
}

func TestSchemaCheck_ControlPlaneRefusesBadRegistration(t *testing.T) {
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
		t.Fatal(err)
	}
	gw.Handler()
	_, err := gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{
			{Namespace: "fine", Version: "v1", ProtoSource: []byte(checkOtherProto)},
			{Namespace: "colb", Version: "v1", ProtoSource: []byte(checkBadProto)},
		},
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "schema does not build") {
		t.Fatalf("Register: err = %v, want InvalidArgument schema build refusal", err)
	}
	gw.mu.Lock()
	n := len(gw.slots)
	_, fine := gw.slots[poolKey{namespace: "fine", version: "v1"}]
	gw.mu.Unlock()
	if fine {
		t.Fatal("the good binding of a refused Register joined")
	}
	if got, want := checkQuery(t, gw, `{ col { get { a } } }`), `{"data":{"col":{"get":{"a":""}}}}`; got != want {
		t.Fatalf("query: %s, want %s (slots=%d)", got, want, n)
	}
}

// TestSchemaCheck_Cluster: on a 2-node cluster both nodes give an
// enum the same renamed values, a registration on A that cannot build
// is refused there and never reaches B, and a registry entry that
// cannot build (written as another node would) is skipped by both
// reconcilers while the other services keep serving.
func TestSchemaCheck_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	// Values deliberately out of sorted order.
	spec := strings.Replace(enumRenameSpec, `["a-b","a_b","true","1st","ok","__x",""]`, `["ok","a-b","","true","a_b","__x","1st"]`, 1)
	clusterRegisterOn(t, a, &cpv1.RegisterRequest{
		Addr:     "http://127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{{Namespace: "e", Version: "v1", OpenapiSpec: []byte(spec)}},
	})
	clusterRegisterOn(t, a, &cpv1.RegisterRequest{
		Addr:     "127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{{Namespace: "col", Version: "v1", ProtoSource: []byte(checkGoodProto)}},
	})
	for _, n := range []*clusterNode{a, b} {
		waitForSlotIR(t, n.gw, "e", "v1")
		waitForSlotIR(t, n.gw, "col", "v1")
	}
	const enumQ = `{ __type(name: "e_Color") { enumValues { name } } }`
	wantEnum := `{"data":{"__type":{"enumValues":[{"name":"TRUE"},{"name":"_"},{"name":"_1st"},{"name":"_x"},{"name":"a_b"},{"name":"a_b_2"},{"name":"ok"}]}}}`
	for _, n := range []*clusterNode{a, b} {
		if got := sortedEnumValues(t, checkQuery(t, n.gw, enumQ)); got != wantEnum {
			t.Fatalf("node %s enum values:\n got %s\nwant %s", n.cluster.NodeID, got, wantEnum)
		}
	}

	// Refused on A: no KV entry, nothing on B.
	_, err := a.gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:     "127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{{Namespace: "colb", Version: "v1", ProtoSource: []byte(checkBadProto)}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad Register on A: err = %v, want InvalidArgument", err)
	}
	kv := a.gw.registryKV()
	keys, _ := kv.Keys(context.Background())
	for _, k := range keys {
		if strings.HasPrefix(k, "pool.colb.") {
			t.Fatalf("refused registration wrote %s", k)
		}
	}

	// A bad entry in the registry (as a node without the check would
	// write it): both reconcilers refuse it and keep serving.
	val, _ := json.Marshal(registryValue{
		RegID: "rogue", Namespace: "colb", Version: "v1", ReplicaID: "r1",
		Addr: "127.0.0.1:1", OwnerNodeID: "elsewhere", Hash: make([]byte, 32),
		ProtoSource: []byte(checkBadProto),
	})
	if _, err := kv.Put(context.Background(), replicaKey("colb", "v1", "r1"), val); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kv.Delete(context.Background(), replicaKey("colb", "v1", "r1")) })
	// The reconcilers have seen the put once the gate records it.
	for _, n := range []*clusterNode{a, b} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			n.gw.mu.Lock()
			_, seen := n.gw.gate.instances[replicaKey("colb", "v1", "r1")]
			n.gw.mu.Unlock()
			if seen {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("node %s never saw the rogue entry", n.cluster.NodeID)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond)
	for _, n := range []*clusterNode{a, b} {
		n.gw.mu.Lock()
		_, has := n.gw.slots[poolKey{namespace: "colb", version: "v1"}]
		n.gw.mu.Unlock()
		if has {
			t.Fatalf("node %s joined a registry entry that cannot build", n.cluster.NodeID)
		}
		if got := sortedEnumValues(t, checkQuery(t, n.gw, enumQ)); got != wantEnum {
			t.Fatalf("node %s after rogue entry: %s", n.cluster.NodeID, got)
		}
		// A later good registration still builds on every node.
	}
	clusterRegisterOn(t, b, &cpv1.RegisterRequest{
		Addr:     "127.0.0.1:1",
		Services: []*cpv1.ServiceBinding{{Namespace: "other", Version: "v1", ProtoSource: []byte(checkOtherProto)}},
	})
	for _, n := range []*clusterNode{a, b} {
		waitForSlotIR(t, n.gw, "other", "v1")
	}
}

func clusterRegisterOn(t *testing.T, n *clusterNode, req *cpv1.RegisterRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var lastErr error
	for ctx.Err() == nil {
		resp, err := n.gw.ControlPlane().Register(ctx, req)
		if err == nil && resp.GetRegistrationId() != "" {
			return
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Register never succeeded: %v", lastErr)
}

func waitForSlotIR(t *testing.T, gw *Gateway, ns, ver string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		gw.mu.Lock()
		s := gw.slots[poolKey{namespace: ns, version: ver}]
		ok := s != nil && s.ir != nil && gw.schema.Load() != nil
		gw.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot %s/%s never appeared", ns, ver)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkConn answers every unary call with an empty reply.
type checkConn struct{}

func (checkConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error { return nil }

func (checkConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "no streams")
}

// A full build that fails after the check let a slot in (here the
// slot is put in place directly, as a clash the check cannot see
// would leave it) keeps the last good schema live, and logs and
// counts the error once, not on every rebuild.
func TestSchemaCheck_FullBuildFailureKeepsSchema(t *testing.T) {
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
		t.Fatal(err)
	}
	gw.Handler()
	fd, err := compileProtoBytes("colb.proto", []byte(checkBadProto), nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := poolKey{namespace: "colb", version: "v1"}
	gw.mu.Lock()
	s := &slot{key: bad, kind: slotKindProto, proto: &pool{file: fd}}
	gw.slots[bad] = s
	gw.bakeSlotIRLocked(s)
	errs := []error{gw.assembleLocked(), gw.assembleLocked()}
	gw.mu.Unlock()
	for _, err := range errs {
		if err == nil {
			t.Fatal("full build with a clashing slot succeeded")
		}
	}
	if got, want := checkQuery(t, gw, `{ col { get { a } } }`), `{"data":{"col":{"get":{"a":""}}}}`; got != want {
		t.Fatalf("query after a failed build: %s, want %s", got, want)
	}
	body := scrapeBody(t, gw)
	wantSeries(t, body, mp+`schema_build_failures_total`, "1")
	wantSeries(t, body, mp+`schema_rebuilds_total{result="error"}`, "2")

	// With the slot gone the next build succeeds and serves both.
	gw.mu.Lock()
	gw.releaseSlotLocked(bad)
	gw.mu.Unlock()
	if err := gw.AddProtoBytes("other.proto", []byte(checkOtherProto), To(checkConn{}), As("other")); err != nil {
		t.Fatalf("registration after the slot left: %v", err)
	}
	if got, want := checkQuery(t, gw, `{ col { get { a } } other { get { a } } }`), `{"data":{"col":{"get":{"a":""}},"other":{"get":{"a":""}}}}`; got != want {
		t.Fatalf("query: %s, want %s", got, want)
	}
	gw.mu.Lock()
	last := gw.lastBuildErr
	gw.mu.Unlock()
	if last != "" {
		t.Fatalf("lastBuildErr = %q after a good build", last)
	}
}

// The name index follows deregistration: once the namespace that owns
// a name leaves, another namespace may take it.
func TestSchemaCheck_NameFreedOnRelease(t *testing.T) {
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
		t.Fatal(err)
	}
	if err := gw.AddProtoBytes("colb.proto", []byte(checkBadProto), To(checkConn{}), As("colb")); err == nil {
		t.Fatal("clash accepted")
	}
	gw.mu.Lock()
	gw.releaseSlotLocked(poolKey{namespace: "col", version: "v1"})
	gw.mu.Unlock()
	if err := gw.AddProtoBytes("colb.proto", []byte(checkBadProto), To(checkConn{}), As("colb")); err != nil {
		t.Fatalf("registration after the owner left: %v", err)
	}
	if got, want := checkQuery(t, gw, `{ colb { put { b } } }`), `{"data":{"colb":{"put":{"b":""}}}}`; got != want {
		t.Fatalf("query: %s, want %s", got, want)
	}
}

// The index covers names the gateway itself makes, not only service
// types: namespace colV1 renders ColV1QueryNamespace, which is also
// the container of col's v1.
func TestSchemaCheck_ContainerNameClash(t *testing.T) {
	gw := metricsGateway(t)
	if err := gw.AddProtoBytes("col.proto", []byte(checkGoodProto), To(checkConn{}), As("col")); err != nil {
		t.Fatal(err)
	}
	err := gw.AddProtoBytes("other.proto", []byte(checkOtherProto), To(checkConn{}), As("colV1"))
	if err == nil || !strings.Contains(err.Error(), "ColV1QueryNamespace") {
		t.Fatalf("err = %v, want a refusal naming ColV1QueryNamespace", err)
	}
}
