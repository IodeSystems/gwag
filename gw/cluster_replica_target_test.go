package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Replica counts across the cluster: buckets and streams are created
// at the count the cluster runs at, raises wait while a node is being
// evacuated, and only ForgetPeer lowers.

type replNode struct {
	name string
	cl   *Cluster
	gw   *Gateway
}

// replTTL is the peers bucket TTL in these tests: a stopped node
// leaves the live set, and can be forgotten, this long after it
// stopped.
const replTTL = 6 * time.Second

func replOptions(cl *Cluster, extra ...Option) []Option {
	return append([]Option{WithCluster(cl), WithoutMetrics(), WithoutBackpressure(), withClusterKVTTL(replTTL)}, extra...)
}

// startReplNodes starts one NATS server and gateway per name, each
// with every other server as a route, and waits until every node sees
// every peer. opts gives each node's extra gateway options.
func startReplNodes(t *testing.T, names []string, opts func(i int) []Option) []*replNode {
	t.Helper()
	addrs := make([]string, len(names))
	for i := range addrs {
		addrs[i] = freeAddr(t)
	}
	var nodes []*replNode
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
		var extra []Option
		if opts != nil {
			extra = opts(i)
		}
		n := &replNode{name: name, cl: cl}
		n.start(t, extra...)
		nodes = append(nodes, n)
	}
	waitLive(t, nodes, len(nodes))
	return nodes
}

// start runs a gateway on the node's NATS server. ControlPlane boots
// cluster tracking in the background.
func (n *replNode) start(t *testing.T, extra ...Option) {
	t.Helper()
	g := New(replOptions(n.cl, extra...)...)
	t.Cleanup(g.Close)
	g.ControlPlane()
	n.gw = g
}

// restart stops the gateway and starts a new one on the same NATS
// server (same node id) with extra options.
func (n *replNode) restart(t *testing.T, extra ...Option) {
	t.Helper()
	n.gw.Close()
	n.start(t, extra...)
}

func (n *replNode) kill() {
	n.gw.Close()
	n.cl.Close()
}

func (n *replNode) tracker() *peerTracker {
	n.gw.mu.Lock()
	defer n.gw.mu.Unlock()
	return n.gw.peers
}

func waitLive(t *testing.T, nodes []*replNode, want int) {
	t.Helper()
	waitUntilErr(t, 60*time.Second, fmt.Sprintf("every node sees %d peers", want), func() error {
		for _, n := range nodes {
			pt := n.tracker()
			if pt == nil {
				return fmt.Errorf("%s: no tracker", n.name)
			}
			if l := len(pt.LivePeers()); l != want {
				return fmt.Errorf("%s: %d live peers", n.name, l)
			}
		}
		return nil
	})
}

// gwagStreams returns every gateway bucket and event stream, by name.
func gwagStreams(t *testing.T, js jetstream.JetStream) map[string]*jetstream.StreamInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := map[string]*jetstream.StreamInfo{}
	l := js.ListStreams(ctx)
	for info := range l.Info() {
		if n := info.Config.Name; strings.HasPrefix(n, "KV_gwag-") || strings.HasPrefix(n, "EV_") {
			out[n] = info
		}
	}
	if err := l.Err(); err != nil {
		t.Fatalf("list streams: %v", err)
	}
	return out
}

// settledAt: every gateway stream is at r replicas, has a leader, no
// move in flight and current followers.
func settledAt(t *testing.T, js jetstream.JetStream, r int) error {
	for name, info := range gwagStreams(t, js) {
		ci := info.Cluster
		if info.Config.Replicas != r {
			return fmt.Errorf("%s: R%d", name, info.Config.Replicas)
		}
		if ci == nil || ci.Leader == "" || ci.Desired != nil || len(ci.Replicas) != r-1 {
			return fmt.Errorf("%s: cluster %+v", name, ci)
		}
		for _, p := range ci.Replicas {
			if !p.Current {
				return fmt.Errorf("%s: %s not current", name, p.Name)
			}
		}
	}
	return nil
}

func streamConfigOf(t *testing.T, js jetstream.JetStream, name string) jetstream.StreamConfig {
	t.Helper()
	cfg, err := readStreamConfig(context.Background(), js, name)
	if err != nil || cfg == nil {
		t.Fatalf("stream %s: %v (missing=%v)", name, err, cfg == nil)
	}
	return *cfg
}

func putEvacuation(t *testing.T, pt *peerTracker, nodeID, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pt.markEvacuating(ctx, nodeID, name); err != nil {
		t.Fatalf("mark evacuating: %v", err)
	}
}

// ensureStream ensures an event stream through g as an index rebuild
// does, and records it as ensured (setEventStreamReplicas covers it).
func ensureStream(g *Gateway, name, subject string) {
	g.mu.Lock()
	replicas := g.eventStreamReplicas()
	pt := g.peers
	g.mu.Unlock()
	st := &g.eventStreams
	st.mu.Lock()
	if st.known == nil {
		st.known = map[string]string{}
	}
	st.known[name] = subject
	st.mu.Unlock()
	g.ensureEventStream(g.cfg.cluster.JS, pt, name, subject, replicas)
}

// TestReplicas_CreatedAtClusterCount: on a cluster at R3, a bucket a
// node creates at start and an event stream it creates start at R3.
// An evacuation record (for a node that does not exist) holds every
// raise back, so R3 can only come from the create.
func TestReplicas_CreatedAtClusterCount(t *testing.T) {
	nodes := startReplNodes(t, []string{"ct-a", "ct-b", "ct-c"}, nil)
	js := nodes[0].cl.JS
	waitUntilErr(t, 60*time.Second, "buckets settled at R3", func() error { return settledAt(t, js, 3) })

	putEvacuation(t, nodes[0].tracker(), "NOSUCHNODE", "no-such-node")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, b := range []string{appKeysBucketName, notificationsBucketName, externalDependentsBucketName} {
		if err := js.DeleteStream(ctx, "KV_"+b); err != nil {
			t.Fatalf("delete %s: %v", b, err)
		}
	}
	nodes[2].restart(t)
	waitLive(t, nodes, 3)
	for _, b := range []string{appKeysBucketName, notificationsBucketName, externalDependentsBucketName} {
		if r := streamConfigOf(t, js, "KV_"+b).Replicas; r != 3 {
			t.Errorf("%s created at R%d, want R3", b, r)
		}
	}
	ensureStream(nodes[1].gw, "EV_ct_v1_Ticks", "ct.ticks.*")
	if r := streamConfigOf(t, js, "EV_ct_v1_Ticks").Replicas; r != 3 {
		t.Errorf("event stream created at R%d, want R3", r)
	}
	if got := bootReplicaTarget(ctx, js); got != 3 {
		t.Errorf("bootReplicaTarget = %d, want 3", got)
	}
}

// TestReplicas_EvacuationHoldsRaisesAndMovesCreates: four nodes at
// R3; one is evacuated. While its record stands no node raises a
// bucket that was lowered by hand, and streams created meanwhile end
// up off the evacuated node (NATS places a new group on any online
// server; the creator evacuates again). ForgetPeer of the stopped
// node clears the record and the raise follows.
func TestReplicas_EvacuationHoldsRaisesAndMovesCreates(t *testing.T) {
	nodes := startReplNodes(t, []string{"ev-a", "ev-b", "ev-c", "ev-d"}, nil)
	js := nodes[0].cl.JS
	waitUntilErr(t, 60*time.Second, "buckets settled at R3", func() error { return settledAt(t, js, 3) })

	gone := nodes[3]
	goneID := gone.cl.NodeID
	ctx := context.Background()
	waitUntilErr(t, 90*time.Second, "ev-d evacuated", func() error {
		_, remaining, done, _, err := nodes[0].gw.evacuatePeer(ctx, goneID, false, false)
		if err != nil {
			return err
		}
		if !done {
			return fmt.Errorf("remaining %v", remaining)
		}
		return nil
	})
	rec, _, err := getEvacuations(ctx, nodes[0].cl.Conn)
	if err != nil || rec.Peers[goneID].Name != "ev-d" {
		t.Fatalf("evacuation record %+v err %v", rec, err)
	}

	// A bucket below the policy is not raised while ev-d is recorded.
	held := "KV_" + stableBucketName
	if _, err := applyStream(ctx, js, held, func() bool { return true }, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
		want := *cur
		want.Replicas = 1
		return want, true
	}); err != nil {
		t.Fatalf("lower %s: %v", held, err)
	}
	for _, n := range nodes {
		n.tracker().reconcileReplicas(ctx)
	}

	// Streams created during the evacuation. At R3 of 4 servers each
	// is placed on ev-d with probability 3/4.
	var created []string
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("EV_ev_v1_S%d", i)
		ensureStream(nodes[i%3].gw, name, fmt.Sprintf("ev.s%d", i))
		created = append(created, name)
	}
	waitUntilErr(t, 60*time.Second, "created streams off ev-d", func() error {
		for _, name := range created {
			on, err := streamNodes(ctx, js, name)
			if err != nil {
				return err
			}
			if slices.Contains(on, "ev-d") {
				return fmt.Errorf("%s on %v", name, on)
			}
		}
		_, remaining, done, _, err := nodes[0].gw.evacuatePeer(ctx, goneID, true, false)
		if err != nil {
			return err
		}
		if !done {
			return fmt.Errorf("check: remaining %v", remaining)
		}
		return nil
	})
	for _, name := range created {
		if r := streamConfigOf(t, js, name).Replicas; r != 3 {
			t.Errorf("%s at R%d, want R3", name, r)
		}
	}
	// Two refresh ticks: still not raised.
	time.Sleep(2 * replTTL / peerTTLPerRefresh)
	if r := streamConfigOf(t, js, held).Replicas; r != 1 {
		t.Fatalf("%s raised to R%d during the evacuation", held, r)
	}

	gone.kill()
	f := nodes[0].gw.ControlPlane()
	waitUntilErr(t, 60*time.Second, "forget ev-d", func() error {
		resp, err := f.ForgetPeer(ctx, &cpv1.ForgetPeerRequest{NodeId: goneID})
		if err != nil {
			return err
		}
		if !resp.GetMetaRemoved() {
			return fmt.Errorf("forget: %+v", resp)
		}
		return nil
	})
	rec, _, err = getEvacuations(ctx, nodes[0].cl.Conn)
	if err != nil || len(rec.Peers) != 0 {
		t.Fatalf("record after forget %+v err %v", rec, err)
	}
	waitUntilErr(t, 60*time.Second, held+" raised after the forget", func() error {
		if r := streamConfigOf(t, js, held).Replicas; r != 3 {
			return fmt.Errorf("R%d", r)
		}
		return nil
	})
}

// TestReplicas_EvacuationWaitsForWholeStreams: an evacuation does not
// start while a managed bucket is below the cluster's replica count:
// it would hold the raise back until the node is forgotten. The
// refusal is FailedPrecondition naming the bucket, writes no record,
// and starts the raise; once the bucket is whole the evacuation runs.
func TestReplicas_EvacuationWaitsForWholeStreams(t *testing.T) {
	nodes := startReplNodes(t, []string{"wh-a", "wh-b", "wh-c", "wh-d"}, nil)
	js := nodes[0].cl.JS
	waitUntilErr(t, 60*time.Second, "buckets settled at R3", func() error { return settledAt(t, js, 3) })

	gone := nodes[3]
	goneID := gone.cl.NodeID
	ctx := context.Background()
	short := "KV_" + appKeysBucketName
	if _, err := applyStream(ctx, js, short, func() bool { return true }, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
		want := *cur
		want.Replicas = 1
		return want, true
	}); err != nil {
		t.Fatalf("lower %s: %v", short, err)
	}

	cp := nodes[0].gw.ControlPlane()
	_, err := cp.EvacuatePeer(ctx, &cpv1.EvacuatePeerRequest{NodeId: goneID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("evacuate with %s at R1: %v, want FailedPrecondition", short, err)
	}
	var subjects []string
	for _, d := range status.Convert(err).Details() {
		if pf, ok := d.(*errdetails.PreconditionFailure); ok {
			for _, v := range pf.GetViolations() {
				subjects = append(subjects, v.GetSubject())
			}
		}
	}
	if !slices.Contains(subjects, short) {
		t.Fatalf("violations %v do not name %s (%v)", subjects, short, err)
	}
	rec, _, err := getEvacuations(ctx, nodes[0].cl.Conn)
	if err != nil || len(rec.Peers) != 0 {
		t.Fatalf("record after a refused evacuation %+v err %v", rec, err)
	}

	// Polled as `gwag peer evacuate --wait` does: refused until the
	// raise is applied, then evacuated.
	waitUntilErr(t, 90*time.Second, "wh-d evacuated", func() error {
		resp, err := cp.EvacuatePeer(ctx, &cpv1.EvacuatePeerRequest{NodeId: goneID})
		if err != nil {
			return err
		}
		if !resp.GetDone() {
			return fmt.Errorf("remaining %v", resp.GetRemaining())
		}
		return nil
	})
	if r := streamConfigOf(t, js, short).Replicas; r != 3 {
		t.Fatalf("%s at R%d after the evacuation, want R3", short, r)
	}
	on, err := streamNodes(ctx, js, short)
	if err != nil || slices.Contains(on, "wh-d") {
		t.Fatalf("%s on %v err %v", short, on, err)
	}
	// Renewals of a started evacuation are not checked again.
	if _, err := cp.EvacuatePeer(ctx, &cpv1.EvacuatePeerRequest{NodeId: goneID}); err != nil {
		t.Fatalf("renew: %v", err)
	}
}

// TestReplicas_ExpiredEvacuationNoLongerHolds: a record past its
// expiry does not count, so a crashed evacuate does not block raises.
func TestReplicas_ExpiredEvacuationNoLongerHolds(t *testing.T) {
	f := newForgetPeerFixture(t)
	pt := f.gw.peers
	ctx := context.Background()
	b, _ := json.Marshal(evacuationRecord{Peers: map[string]evacuatingPeer{
		"OLD": {Name: "old", UntilUnixMs: time.Now().Add(-time.Second).UnixMilli()},
		"NEW": {Name: "new", UntilUnixMs: time.Now().Add(time.Minute).UnixMilli()},
	}})
	if _, err := pt.cluster.Put(ctx, clusterEvacuationsKey, b); err != nil {
		t.Fatal(err)
	}
	m, err := pt.evacuations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["OLD"]; ok || len(m) != 1 {
		t.Fatalf("active %v, want only NEW", m)
	}
	if !pt.evacuating(ctx) {
		t.Fatal("NEW is active")
	}
	if err := pt.clearEvacuating(ctx, "NEW"); err != nil {
		t.Fatal(err)
	}
	if pt.evacuating(ctx) {
		t.Fatal("evacuating after clear")
	}
}

// TestReplicas_ForgetPeerLowersEverything: three nodes at R3, one
// with the Schema Registry. A node stops: after its entry expires
// nothing is lowered (a brief loss does not shrink). ForgetPeer
// through a node without the Schema Registry lowers every bucket,
// gwag-schemas and gwag-cluster included, and every event stream.
func TestReplicas_ForgetPeerLowersEverything(t *testing.T) {
	nodes := startReplNodes(t, []string{"fg-a", "fg-b", "fg-c"}, func(i int) []Option {
		if i == 0 {
			return []Option{WithSchemaRegistry(SchemaRegistryOptions{})}
		}
		return nil
	})
	js := nodes[1].cl.JS
	for _, n := range nodes {
		ensureStream(n.gw, "EV_fg_v1_Ticks", "fg.ticks.*")
	}
	waitUntilErr(t, 60*time.Second, "everything settled at R3", func() error { return settledAt(t, js, 3) })
	all := gwagStreams(t, js)
	for _, want := range []string{"KV_" + schemasBucketName, "KV_" + clusterBucketName, "KV_" + appKeysBucketName, "EV_fg_v1_Ticks"} {
		if all[want] == nil {
			t.Fatalf("%s missing; have %v", want, keysOf(all))
		}
	}

	stopped := nodes[2]
	stoppedID := stopped.cl.NodeID
	stopped.kill()
	survivors := nodes[:2]
	waitLive(t, survivors, 2)
	// Brief loss: the live set shrank, the replica count does not.
	for _, n := range survivors {
		n.tracker().reconcileReplicas(context.Background())
	}
	time.Sleep(2 * replTTL / peerTTLPerRefresh)
	for name, info := range gwagStreams(t, js) {
		if info.Config.Replicas != 3 {
			t.Fatalf("%s at R%d after a peer loss without forget", name, info.Config.Replicas)
		}
	}

	resp, err := nodes[1].gw.ControlPlane().ForgetPeer(context.Background(), &cpv1.ForgetPeerRequest{NodeId: stoppedID})
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if resp.GetNewReplicas() != 1 {
		t.Fatalf("forget: %+v, want new_replicas 1 (odd, 2 left)", resp)
	}
	for name, info := range gwagStreams(t, js) {
		if info.Config.Replicas != 1 {
			t.Errorf("%s at R%d after forget, want R1", name, info.Config.Replicas)
		}
	}
	// And it stays there: 2 live under odd is R1 on both nodes.
	time.Sleep(2 * replTTL / peerTTLPerRefresh)
	for name, info := range gwagStreams(t, js) {
		if info.Config.Replicas != 1 {
			t.Errorf("%s back at R%d", name, info.Config.Replicas)
		}
	}
}

func keysOf(m map[string]*jetstream.StreamInfo) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestReplicas_PolicyChangeAtRestart: two nodes under odd run R1. A
// node restarted with auto raises to R2; restarted with odd again it
// does not lower, and two nodes with different policies do not move
// the count back and forth.
func TestReplicas_PolicyChangeAtRestart(t *testing.T) {
	nodes := startReplNodes(t, []string{"pc-a", "pc-b"}, nil)
	js := nodes[0].cl.JS
	waitUntilErr(t, 60*time.Second, "settled at R1", func() error { return settledAt(t, js, 1) })

	nodes[1].restart(t, WithClusterReplicas(ClusterReplicasAuto))
	waitLive(t, nodes, 2)
	waitUntilErr(t, 60*time.Second, "auto raises to R2", func() error { return settledAt(t, js, 2) })

	// Mixed policies: a's odd gives 1, b's auto gives 2. Neither lowers.
	time.Sleep(2 * replTTL / peerTTLPerRefresh)
	if err := settledAt(t, js, 2); err != nil {
		t.Fatalf("mixed policies: %v", err)
	}

	nodes[1].restart(t)
	waitLive(t, nodes, 2)
	for _, n := range nodes {
		n.tracker().reconcileReplicas(context.Background())
	}
	time.Sleep(2 * replTTL / peerTTLPerRefresh)
	if err := settledAt(t, js, 2); err != nil {
		t.Fatalf("restart with odd lowered: %v", err)
	}
}
