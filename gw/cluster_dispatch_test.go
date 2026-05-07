package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	cpv1 "github.com/iodesystems/go-api-gateway/gw/proto/controlplane/v1"
	greeterv1 "github.com/iodesystems/go-api-gateway/examples/multi/gen/greeter/v1"
)

// freeAddr grabs a free 127.0.0.1 port and returns its address. Closes
// the listener immediately — the caller will rebind. Small race window
// exists but is acceptable for tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// clusterNode wraps a single gateway + its embedded cluster + the
// real grpc.Server hosting its control plane. Cleanup is t.Cleanup-
// driven; the gateway is closed before the cluster (LIFO).
type clusterNode struct {
	cluster   *Cluster
	gw        *Gateway
	cpAddr    string
	httpSrv   *httptest.Server
	grpcSrv   *grpc.Server
	dataDir   string
}

func startClusterNode(t *testing.T, name, clusterAddr string, peers []string) *clusterNode {
	t.Helper()
	dir := t.TempDir()
	// Use a pre-allocated free port for ClientListen too: passing
	// port=0 makes natsd fall back to its default 4222, which collides
	// when a second node starts in the same test.
	cluster, err := StartCluster(ClusterOptions{
		NodeName:      name,
		ClientListen:  freeAddr(t),
		ClusterListen: clusterAddr,
		Peers:         peers,
		DataDir:       dir,
		StartTimeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("[%s] StartCluster: %v", name, err)
	}
	t.Cleanup(cluster.Close)

	gw := New(WithCluster(cluster), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)

	// Bring up the control plane gRPC listener so other nodes (and the
	// test) can call Register / Heartbeat against this gateway. We don't
	// strictly need this for cross-node dispatch (the reconciler reads
	// from the registry KV directly), but it mirrors a real deployment.
	cpAddr := freeAddr(t)
	cpLis, err := net.Listen("tcp", cpAddr)
	if err != nil {
		t.Fatalf("[%s] cp listen: %v", name, err)
	}
	grpcSrv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(grpcSrv, gw.ControlPlane())
	go func() { _ = grpcSrv.Serve(cpLis) }()
	t.Cleanup(grpcSrv.Stop)

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	return &clusterNode{
		cluster: cluster, gw: gw, cpAddr: cpAddr,
		httpSrv: srv, grpcSrv: grpcSrv, dataDir: dir,
	}
}

// startTwoNodeCluster brings up two peering nodes and waits for both
// peer trackers (and reconcilers) to be ready.
func startTwoNodeCluster(t *testing.T) (a, b *clusterNode) {
	t.Helper()
	addrA := freeAddr(t)
	addrB := freeAddr(t)

	a = startClusterNode(t, "A", addrA, []string{addrB})
	b = startClusterNode(t, "B", addrB, []string{addrA})

	for _, n := range []*clusterNode{a, b} {
		// Wait-for-JS is bounded; it just polls.
		jsCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := n.cluster.WaitForJetStream(jsCtx); err != nil {
			cancel()
			t.Fatalf("WaitForJetStream %s: %v", n.cluster.NodeID, err)
		}
		cancel()
		// startClusterTracking captures its ctx as the parent of the
		// long-running watch + reconciler goroutines. Use Background so
		// they don't die when this test helper returns. Cleanup happens
		// via gw.Close → tracker.stop.
		if _, err := n.gw.startClusterTracking(context.Background()); err != nil {
			t.Fatalf("startClusterTracking %s: %v", n.cluster.NodeID, err)
		}
	}

	// Wait for each side to see the other in its live peer set —
	// that's how we know the watch loops have caught up.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(a.gw.peers.LivePeers()) >= 2 && len(b.gw.peers.LivePeers()) >= 2 {
			return a, b
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("peer convergence timeout: A live=%v B live=%v",
		a.gw.peers.LivePeers(), b.gw.peers.LivePeers())
	return nil, nil
}

// registerGreeterOn writes a Register payload directly through the
// in-process control-plane impl. Retries on transient KV failures —
// stream leader election after R=2 bump can take a few seconds in a
// freshly-formed cluster.
func registerGreeterOn(t *testing.T, n *clusterNode, serviceAddr string) {
	t.Helper()
	fdsBytes, err := marshalFileDescriptorSet(greeterv1.File_greeter_proto)
	if err != nil {
		t.Fatalf("marshal FDS: %v", err)
	}
	// Sanity: ensure the FDS round-trips through protobuf so the wire
	// shape matches what controlclient sends.
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(fdsBytes, fds); err != nil {
		t.Fatalf("unmarshal FDS: %v", err)
	}

	req := &cpv1.RegisterRequest{
		Addr:       serviceAddr,
		InstanceId: "greeter-instance",
		Services: []*cpv1.ServiceBinding{
			{
				Namespace:         "greeter",
				Version:           "v1",
				FileDescriptorSet: fdsBytes,
				FileName:          string(greeterv1.File_greeter_proto.Path()),
			},
		},
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := n.gw.ControlPlane().Register(context.Background(), req)
		if err == nil && resp.GetRegistrationId() != "" {
			return
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("Register never succeeded: %v", lastErr)
}

// waitForPool polls until the gateway's local pool registry contains
// (ns, ver). Returns the pool or fatals.
func waitForPool(t *testing.T, gw *Gateway, ns, ver string, timeout time.Duration) *pool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		p, ok := gw.pools[poolKey{namespace: ns, version: ver}]
		gw.mu.Unlock()
		if ok && p.replicaCount() > 0 {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pool %s/%s never appeared on gateway", ns, ver)
	return nil
}

func startGreeterServer(t *testing.T) (*fakeGreeterServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("greeter listen: %v", err)
	}
	greeter := &fakeGreeterServer{}
	grpcSrv := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(grpcSrv, greeter)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)
	return greeter, lis.Addr().String()
}

func TestClusterE2E_CrossGatewayDispatch(t *testing.T) {
	a, b := startTwoNodeCluster(t)
	greeter, greeterAddr := startGreeterServer(t)

	// Register the service against gateway A. KV write replicates
	// (or is at least readable from B via JS routing); B's reconciler
	// picks it up and dials greeterAddr.
	registerGreeterOn(t, a, greeterAddr)

	// Wait for B's reconciler to pick up the registry KV write and
	// dial the greeter address. Tier-1 invariant: cross-gateway
	// dispatch goes through KV → reconciler → handlePut → joinPool.
	waitForPool(t, b.gw, "greeter", "v1", 30*time.Second)

	// Dispatch through gateway B. The gRPC call must land on the
	// real greeter server even though B never received the Register.
	resp, err := http.Post(b.httpSrv.URL+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ greeter { hello(name:\"cluster\") { greeting } } }"}`))
	if err != nil {
		t.Fatalf("post B: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	rr := httptest.NewRecorder()
	if _, err := rr.Body.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	var out struct {
		Data struct {
			Greeter struct {
				Hello map[string]any `json:"hello"`
			} `json:"greeter"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v: %s", err, rr.Body.String())
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors via B: %v", out.Errors)
	}
	if got := out.Data.Greeter.Hello["greeting"]; got != "hello cluster" {
		t.Fatalf("greeting via B = %v, want %q", got, "hello cluster")
	}
	if got := greeter.helloCalls.Load(); got != 1 {
		t.Fatalf("greeter Hello called %d times, want 1", got)
	}
	if got := greeter.lastReq.Load().GetName(); got != "cluster" {
		t.Fatalf("backend got name=%q want cluster", got)
	}

	// Sanity: the same query through A also works (A had the original
	// Register, so its pool was populated synchronously).
	resp2, err := http.Post(a.httpSrv.URL+"/graphql", "application/json",
		strings.NewReader(`{"query":"{ greeter { hello(name:\"local\") { greeting } } }"}`))
	if err != nil {
		t.Fatalf("post A: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("A status=%d", resp2.StatusCode)
	}
	if got := greeter.helloCalls.Load(); got != 2 {
		t.Fatalf("after both calls: helloCalls=%d, want 2", got)
	}
}

