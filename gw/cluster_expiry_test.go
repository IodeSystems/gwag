package gateway

import (
	"context"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"

	"github.com/iodesystems/gwag/v2/gw/controlclient"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// expiryTTL is the peers + registry bucket TTL in these tests, and
// the registration TTL the clients ask for. It must be longer than a
// registry watch stalls after a node dies (measured: about 12s, when
// the watch's consumer lived on that node). A survivor whose watch
// stalls longer than the TTL loses the old entry before it sees the
// failed-over one — as the defaults (30s) are meant to prevent.
const expiryTTL = 20 * time.Second

// expiryBound is how long after the TTL every surviving node must
// agree. It covers JetStream electing a new stream leader after the
// kill (the leader writes the delete markers) and a stalled watch.
const expiryBound = 15 * time.Second

// withClusterKVTTL sets the peers + registry bucket TTL.
func withClusterKVTTL(d time.Duration) Option {
	return func(cfg *config) { cfg.clusterKVTTL = d }
}

type expiryNode struct {
	name    string
	cluster *Cluster
	gw      *Gateway
	grpcSrv *grpc.Server
	cpAddr  string
}

// kill takes the node down the way SIGKILL does as far as the rest of
// the cluster can tell: the control plane stops answering and the
// NATS server leaves, so nothing it held is refreshed or deleted.
func (n *expiryNode) kill() {
	n.grpcSrv.Stop()
	n.cluster.Close()
	n.gw.Close()
}

// startExpiryCluster starts three nodes with expiryTTL buckets and
// waits until each has seen every peer and raised the buckets to R3,
// so one node's loss leaves a quorum.
func startExpiryCluster(t *testing.T) []*expiryNode {
	t.Helper()
	names := []string{"exp-a", "exp-b", "exp-c"}
	addrs := []string{freeAddr(t), freeAddr(t), freeAddr(t)}
	var nodes []*expiryNode
	for i, name := range names {
		var seeds []string
		for j, a := range addrs {
			if j != i {
				seeds = append(seeds, a)
			}
		}
		cl, err := StartCluster(ClusterOptions{
			NodeName:      name,
			ClientListen:  freeAddr(t),
			ClusterListen: addrs[i],
			Peers:         seeds,
			DataDir:       t.TempDir(),
			StartTimeout:  15 * time.Second,
			LogLevel:      "silent",
		})
		if err != nil {
			t.Fatalf("StartCluster %s: %v", name, err)
		}
		t.Cleanup(cl.Close)
		g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure(), withClusterKVTTL(expiryTTL))
		t.Cleanup(g.Close)
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer()
		cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		nodes = append(nodes, &expiryNode{name: name, cluster: cl, gw: g, grpcSrv: srv, cpAddr: lis.Addr().String()})
	}
	// ControlPlane() boots cluster tracking in the background and
	// retries while JetStream meta settles.
	waitUntilErr(t, 60*time.Second, "every node sees 3 peers at R3", func() error {
		for _, n := range nodes {
			n.gw.mu.Lock()
			pt := n.gw.peers
			n.gw.mu.Unlock()
			if pt == nil {
				return fmt.Errorf("%s: no tracker", n.name)
			}
			if l := len(pt.LivePeers()); l != 3 {
				return fmt.Errorf("%s: %d live peers", n.name, l)
			}
			if r := pt.currentR.Load(); r != 3 {
				return fmt.Errorf("%s: R=%d", n.name, r)
			}
		}
		return nil
	})
	// The move to R3 finished: a leader and two current followers.
	// A kill during the reconfiguration leaves the stream without a
	// leader.
	waitUntilErr(t, 60*time.Second, "registry and peers streams settled at R3", func() error {
		pt := nodes[0].gw.peers
		for _, kv := range []jetstream.KeyValue{pt.reg, pt.peers} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			st, err := kv.Status(ctx)
			cancel()
			if err != nil {
				return err
			}
			si := st.(*jetstream.KeyValueBucketStatus).StreamInfo()
			if si.Cluster == nil || si.Cluster.Leader == "" || si.Cluster.Desired != nil || len(si.Cluster.Replicas) != 2 {
				return fmt.Errorf("%s: cluster %+v", kv.Bucket(), si.Cluster)
			}
			for _, p := range si.Cluster.Replicas {
				if !p.Current {
					return fmt.Errorf("%s: replica %s not current", kv.Bucket(), p.Name)
				}
			}
		}
		return nil
	})
	return nodes
}

func waitUntilErr(t *testing.T, within time.Duration, what string, f func() error) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(within)
	for {
		err := f()
		if err == nil {
			t.Logf("%s: after %s", what, time.Since(start).Round(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s: %v", what, within, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}


// owners is the sorted registration IDs of g's ns/v1 replicas; nil
// when the slot is gone.
func owners(g *Gateway, ns string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.protoSlot(poolKey{namespace: ns, version: "v1"})
	if p == nil {
		return nil
	}
	cur := p.replicas.Load()
	if cur == nil || len(*cur) == 0 {
		return nil
	}
	var out []string
	for _, r := range *cur {
		out = append(out, r.owner)
	}
	sort.Strings(out)
	return out
}

// gateInstances counts g's start-gate instances of ns/v1: the
// registry view that readiness, drain cover and the admin listing
// read.
func gateInstances(g *Gateway, ns string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gateInstancesLocked()[poolKey{namespace: ns, version: "v1"}])
}

// selfRegister registers ns (from <ns>.proto) through addr, with more
// as the client's failover addresses.
func selfRegister(t *testing.T, ns, addr string, more ...string) *controlclient.Registration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddr:       addr,
		GatewayAddrs:      more,
		ServiceAddr:       "127.0.0.1:1", // never dialled here
		InstanceID:        ns + "-expiry",
		TTL:               expiryTTL,
		HeartbeatInterval: 500 * time.Millisecond,
		Logger:            func(f string, a ...any) { t.Logf(ns+": "+f, a...) },
		Services: []controlclient.Service{{
			Namespace:   ns,
			Version:     "v1",
			ProtoSource: testProtoBytes(t, ns+".proto"),
		}},
	})
	if err != nil {
		t.Fatalf("SelfRegister %s: %v", ns, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = reg.Close(ctx)
	})
	return reg
}

// TestClusterRegistryExpiry_OwnerNodeDies: the node that took two
// registrations dies.
//
//   - greeter's client knows only that node. Its registry entries
//     expire, and every surviving node drops greeter, its instance and
//     the dead peer within the TTL plus a bound. Before delete markers
//     the survivors kept greeter forever (a TTL expiry raised no watch
//     event), while a node that replayed the registry later did not
//     have it.
//   - library's client knows every node. It registers again on a
//     survivor, no surviving node stops routing library at any point,
//     and once the dead node's entries expire only the new
//     registration is left.
//
// One cluster for both: forming it takes most of the test's time.
func TestClusterRegistryExpiry_OwnerNodeDies(t *testing.T) {
	nodes := startExpiryCluster(t)
	owner, survivors := nodes[0], nodes[1:]
	selfRegister(t, "greeter", owner.cpAddr)
	library := selfRegister(t, "library", owner.cpAddr, survivors[0].cpAddr, survivors[1].cpAddr)
	oldLibrary := library.ID()
	waitUntilErr(t, 15*time.Second, "every node routes greeter and library", func() error {
		for _, n := range nodes {
			for _, ns := range []string{"greeter", "library"} {
				if o := owners(n.gw, ns); len(o) != 1 {
					return fmt.Errorf("%s: %s owners %v", n.name, ns, o)
				}
			}
		}
		return nil
	})

	owner.kill()
	killedAt := time.Now()
	since := func() time.Duration { return time.Since(killedAt).Round(time.Millisecond) }
	end := killedAt.Add(expiryTTL + expiryBound)
	logged := map[string]string{}
	var greeterGoneAt time.Duration
	for {
		greeterGone, libraryMoved := true, library.ID() != oldLibrary
		for _, n := range survivors {
			lib := owners(n.gw, "library")
			if lib == nil {
				t.Fatalf("%s stopped routing library %s after the kill", n.name, since())
			}
			if s := fmt.Sprint(lib); logged[n.name] != s {
				logged[n.name] = s
				t.Logf("%s after the kill: %s routes library to %v (client registration %s)", since(), n.name, lib, library.ID())
			}
			libraryMoved = libraryMoved && len(lib) == 1 && lib[0] == library.ID() && gateInstances(n.gw, "library") == 1
			greeterGone = greeterGone && owners(n.gw, "greeter") == nil && gateInstances(n.gw, "greeter") == 0 && len(n.gw.livePeers()) == 2
		}
		if greeterGone && greeterGoneAt == 0 {
			greeterGoneAt = since()
			t.Logf("%s after the kill: survivors dropped greeter, its instance and the dead peer (TTL %s)", greeterGoneAt, expiryTTL)
		}
		if greeterGone && libraryMoved {
			break
		}
		if time.Now().After(end) {
			for _, n := range survivors {
				t.Errorf("%s: greeter owners %v, %d instances; library owners %v (want only %s), %d instances; %d live peers",
					n.name, owners(n.gw, "greeter"), gateInstances(n.gw, "greeter"),
					owners(n.gw, "library"), library.ID(), gateInstances(n.gw, "library"), len(n.gw.livePeers()))
			}
			t.Fatalf("survivors did not agree within %s of the kill", expiryTTL+expiryBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if library.GatewayAddr() == owner.cpAddr {
		t.Fatalf("library's client is still on the dead gateway %s", owner.cpAddr)
	}
}
