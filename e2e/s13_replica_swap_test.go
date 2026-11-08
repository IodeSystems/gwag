//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/iodesystems/gwag/v2/e2e/harness"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

// s13Tag is the release the blue nodes run. It predates feature
// levels: its peers entries carry no levels and it cannot check the
// cluster's level.
const s13Tag = "v2.11.0"

// s13SwapProto declares one named event channel, so the green nodes
// keep a JetStream stream for it (EV_swap_v1_Ticks). Nothing calls
// the service.
const s13SwapProto = `syntax = "proto3";
package swap.v1;
import "gw/proto/ps/v1/options.proto";
service Swap { rpc Noop(Tick) returns (Tick); }
message Tick {
  option (gwag.ps.v1.binding) = { name: "Ticks" pattern: "swap.ticks.{n}" };
  int64 n = 1;
}
`

const s13Stream = "EV_swap_v1_Ticks"

// TestS13ReplicaSwap upgrades a 3-node cluster by replica swap under
// load: three blue nodes run the v2.11.0 gateway, three green nodes at
// HEAD join one at a time, the blue nodes are evacuated, stopped and
// forgotten one at a time, and the cluster is finalized. GraphQL
// traffic runs through a health-checking LB the whole time, a service
// is registered, and events are published into a JetStream stream.
//
// v2.11.0 cannot raise the feature level, and finalize is refused
// while any v2.11.0 node is live, so the order is join, evacuate +
// drain + forget, then finalize (not finalize first).
func TestS13ReplicaSwap(t *testing.T) {
	env := harness.New(t)
	ctx := harness.Ctx(t, 20*time.Minute)
	old := harness.ReleaseBinary(t, s13Tag, "gateway")

	cluster := env.NamedCluster([]string{"b1", "b2", "b3"}, harness.GatewayOpts{Binary: old, Legacy: true})
	blue := append([]*harness.Gateway(nil), cluster.Nodes...)
	var green []*harness.Gateway
	for _, n := range []string{"g1", "g2", "g3"} {
		green = append(green, cluster.NewNode(harness.GatewayOpts{Name: n}))
	}
	g1 := green[0]

	// The service registers on b1 and knows every other control
	// plane, green first: when b1 goes the registration lands on a
	// node that stays.
	orders := env.Fixture(blue[0], harness.FixtureOpts{Namespace: "orders", NoWait: true,
		FailoverGateways: append(append([]*harness.Gateway(nil), green...), blue[1], blue[2])})
	orders.Proc.WaitLog("FIXTURE ready", 30*time.Second)
	for _, b := range blue {
		b := b
		harness.Eventually(t, 30*time.Second, b.Name+" routes orders", func() error {
			_, err := orders.EchoVia(ctx, b, "v1", "warm")
			return err
		})
	}
	js := s13JetStream(t, cluster.NATSURLs())
	// The registering node writes the stable mark asynchronously
	// after the slot exists, so routing can come first.
	var stableBefore map[string]string
	harness.Eventually(t, 15*time.Second, "stable mark for orders before the swap", func() error {
		stableBefore = s13Bucket(t, js, "gwag-stable")
		if stableBefore["orders"] == "" {
			return fmt.Errorf("no orders mark: %v", stableBefore)
		}
		return nil
	})

	lb := cluster.LB(t, harness.LBOpts{})
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 10 * time.Millisecond},
		func(ctx context.Context, _ int) error {
			return lb.Do(ctx, func(ctx context.Context, g *harness.Gateway) error {
				_, err := orders.EchoVia(ctx, g, "v1", "swap")
				return err
			})
		})
	load.WaitOK(t, 100, 30*time.Second)

	// ---- green joins, one node at a time -----------------------------
	running := []string{"b1", "b2", "b3"}
	for _, g := range green {
		cluster.StartNode(g)
		running = append(running, g.Name)
		cluster.WaitNodes(t, running, 60*time.Second)
		want := len(running)
		harness.Eventually(t, 60*time.Second, g.Name+" is in the blue JetStream meta group", func() error {
			v, err := g.PeersView(ctx)
			if err != nil {
				return err
			}
			if v.MetaSize != want || v.MetaLeader == "" {
				return fmt.Errorf("meta leader=%q size=%d, want size %d", v.MetaLeader, v.MetaSize, want)
			}
			return nil
		})
		lb.WaitInRotation(t, g, 60*time.Second)
	}
	t.Logf("six nodes: %s", lb.StatsString())

	t.Run("expand: green runs at the blue level", func(t *testing.T) {
		v, err := g1.PeersView(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if v.ActiveLevel != 0 {
			t.Fatalf("active level %d, want 0 with v2.11.0 peers", v.ActiveLevel)
		}
		for _, b := range blue {
			p := v.Peer(b.Name)
			if p == nil || p.Version != "" || p.MaxLevel != 0 {
				t.Fatalf("blue peer %s: %+v", b.Name, p)
			}
		}
		for _, g := range green {
			p := v.Peer(g.Name)
			if p == nil || p.Version == "" || p.MaxLevel < 1 {
				t.Fatalf("green peer %s: %+v", g.Name, p)
			}
		}
		res, status, err := g1.Finalize(ctx, 0, true)
		if err != nil || len(res.Blockers) != 3 || res.ActiveLevel != 0 {
			t.Fatalf("dry run: %+v status=%d err=%v", res, status, err)
		}
		if _, status, err := g1.Finalize(ctx, 0, false); status != http.StatusConflict {
			t.Fatalf("finalize with blue live: status %d err %v", status, err)
		}
		// odd caps at 3 with six peers.
		if r := s13Replicas(t, js, "KV_gwag-registry"); r != 3 {
			t.Fatalf("KV_gwag-registry replicas %d with six peers", r)
		}
		if d := s13MarkerTTL(t, js, "KV_gwag-registry"); d != 0 {
			t.Fatalf("delete markers on at level 0 (%s): v2.11.0 replica updates would fail", d)
		}
	})

	// State only HEAD has: an app key, and an event channel whose
	// stream the green nodes keep.
	var key struct {
		AppKey struct {
			ID string `json:"id"`
		} `json:"appKey"`
	}
	if err := g1.Post(ctx, "/api/admin/app-keys", map[string]any{"namespaces": []string{"orders"}, "ttl": "30d", "note": "s13"}, &key, harness.Bearer(g1.Token())); err != nil {
		t.Fatalf("create app key: %v", err)
	}
	root, err := harness.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	swapReg := s11Register(t, ctx, controlclient.Options{
		GatewayAddr:  g1.CPAddr(),
		GatewayAddrs: []string{green[1].CPAddr(), green[2].CPAddr()},
		ServiceAddr:  s11GRPCServer(t),
		InstanceID:   "swap-1",
		Services: []controlclient.Service{{Namespace: "swap", Version: "v1", ProtoSource: []byte(s13SwapProto),
			ProtoImports: map[string][]byte{"gw/proto/ps/v1/options.proto": s11Read(t, root, "gw/proto/ps/v1/options.proto")}}},
	})
	s11WaitReg(t, swapReg, "swap-1", 30*time.Second)
	cluster.WaitStreamReplicas(t, 3, 60*time.Second, s13Stream)
	pub := s13StartPublisher(t, cluster.NATSURLs())
	pub.waitAcked(t, 20, 30*time.Second)

	// ---- blue leaves, one node at a time -----------------------------
	var evacuated []string
	for i, b := range blue {
		id := b.NodeID(t)
		lb.Pull(b)
		lb.WaitIdle(t, b, 10*time.Second)
		s13LogPlacement(t, js, "before evacuating "+b.Name)
		ev := g1.Evacuate(t, id, false, 2*time.Minute)
		t.Logf("%s evacuated (%s)", b.Name, ev.Name)
		evacuated = append(evacuated, b.Name)
		s13RequireR3Off(t, js, evacuated)
		b.Proc.Stop(30 * time.Second)
		f := g1.Forget(t, id, 2*time.Minute)
		if !f.MetaRemoved || f.NewReplicas != 3 {
			t.Fatalf("forget %s: %+v", b.Name, f)
		}
		running = running[1:]
		cluster.WaitNodes(t, running, 60*time.Second)
		if i == 0 {
			// After the first blue node goes the registration has moved.
			harness.Eventually(t, 60*time.Second, "orders routes after b1 left", func() error {
				_, err := orders.EchoVia(ctx, g1, "v1", "moved")
				return err
			})
		}
		t.Logf("%s gone: %s", b.Name, lb.StatsString())
	}
	harness.Eventually(t, 30*time.Second, "meta group is the three green nodes", func() error {
		v, err := g1.PeersView(ctx)
		if err != nil {
			return err
		}
		if v.MetaSize != 3 || len(v.MetaOffline) != 0 {
			return fmt.Errorf("meta size=%d offline=%v", v.MetaSize, v.MetaOffline)
		}
		return nil
	})

	// ---- contract ----------------------------------------------------
	res, status, err := g1.Finalize(ctx, 0, false)
	if err != nil || res.PreviousLevel != 0 || res.ActiveLevel < 1 {
		t.Fatalf("finalize: %+v status=%d err=%v", res, status, err)
	}
	for _, g := range green {
		g := g
		harness.Eventually(t, 30*time.Second, g.Name+" at the new level", func() error {
			v, err := g.PeersView(ctx)
			if err != nil {
				return err
			}
			if v.ActiveLevel != res.ActiveLevel {
				return fmt.Errorf("level %d", v.ActiveLevel)
			}
			return nil
		})
	}
	harness.Eventually(t, 30*time.Second, "delete markers on after finalize", func() error {
		for _, s := range []string{"KV_gwag-registry", "KV_gwag-peers"} {
			if d := s13MarkerTTL(t, js, s); d <= 0 {
				return fmt.Errorf("%s: markers off", s)
			}
		}
		return nil
	})
	// Traffic keeps flowing at the new level.
	okNow, _ := load.Counts()
	load.WaitOK(t, okNow+100, 30*time.Second)
	acked := pub.stop(t)
	result := load.Stop()
	t.Logf("load: ok=%d failed=%d errors=%v; LB %s", result.OK, result.Failed, result.Errors, lb.StatsString())
	t.Logf("events acked=%d publish retries=%d", acked, pub.retries.Load())

	t.Run("zero failed requests", func(t *testing.T) {
		result.RequireNoFailures(t)
	})
	t.Run("state intact", func(t *testing.T) {
		// swap registered during the swap and adds its own mark.
		after := s13Bucket(t, js, "gwag-stable")
		for k, v := range stableBefore {
			if after[k] != v {
				t.Fatalf("stable mark %s: before %s after %s", k, v, after[k])
			}
		}
		if after["swap"] == "" {
			t.Fatalf("no stable mark for swap: %v", after)
		}
		for _, g := range green {
			var keys struct {
				AppKeys []struct {
					ID string `json:"id"`
				} `json:"appKeys"`
			}
			if err := g.Get(ctx, "/api/admin/app-keys", &keys, harness.Bearer(g.Token())); err != nil {
				t.Fatalf("%s app keys: %v", g.Name, err)
			}
			if len(keys.AppKeys) != 1 || keys.AppKeys[0].ID != key.AppKey.ID {
				t.Fatalf("%s app keys: %+v, want %s", g.Name, keys.AppKeys, key.AppKey.ID)
			}
			for _, ns := range []string{"orders", "swap"} {
				svcs, err := g.Admin().Services(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if svcs.Service(ns, "v1") == nil {
					t.Fatalf("%s lost %s/v1", g.Name, ns)
				}
			}
		}
	})
	t.Run("no event lost", func(t *testing.T) {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		s, err := js.Stream(c, s13Stream)
		if err != nil {
			t.Fatal(err)
		}
		// One more than acked is the publish stop cut before its ack.
		st := s.CachedInfo().State
		if st.Msgs < uint64(acked) || st.Msgs > uint64(acked)+1 || st.LastSeq != st.Msgs || st.FirstSeq != 1 {
			t.Fatalf("stream holds %d messages (seq %d..%d), %d acked", st.Msgs, st.FirstSeq, st.LastSeq, acked)
		}
	})
	t.Run("every stream on green at R3", func(t *testing.T) {
		for name, nodes := range s13Placement(t, js) {
			if len(nodes) != 3 {
				t.Errorf("%s on %v", name, nodes)
			}
			for _, n := range nodes {
				if !strings.HasPrefix(n, "g") {
					t.Errorf("%s has a replica on %s: %v", name, n, nodes)
				}
			}
		}
	})
}

// TestS13bRollbackBeforeFinalize adds two HEAD nodes to a v2.11.0
// cluster and takes them out again before any finalize: the blue
// cluster keeps its level, its streams and its meta quorum. The last
// HEAD node leaves the JetStream meta group itself (evacuate with
// leave), because a v2.11.0 node cannot remove it.
func TestS13bRollbackBeforeFinalize(t *testing.T) {
	env := harness.New(t)
	ctx := harness.Ctx(t, 15*time.Minute)
	old := harness.ReleaseBinary(t, s13Tag, "gateway")

	cluster := env.NamedCluster([]string{"b1", "b2", "b3"}, harness.GatewayOpts{Binary: old, Legacy: true})
	blue := append([]*harness.Gateway(nil), cluster.Nodes...)
	orders := env.Fixture(blue[0], harness.FixtureOpts{Namespace: "orders", NoWait: true,
		FailoverGateways: []*harness.Gateway{blue[1], blue[2]}})
	orders.Proc.WaitLog("FIXTURE ready", 30*time.Second)
	for _, b := range blue {
		b := b
		harness.Eventually(t, 30*time.Second, b.Name+" routes orders", func() error {
			_, err := orders.EchoVia(ctx, b, "v1", "warm")
			return err
		})
	}
	js := s13JetStream(t, cluster.NATSURLs())
	stableBefore := s13Bucket(t, js, "gwag-stable")
	green := []*harness.Gateway{cluster.NewNode(harness.GatewayOpts{Name: "g1"}), cluster.NewNode(harness.GatewayOpts{Name: "g2"})}
	lb := cluster.LB(t, harness.LBOpts{})
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 4, Interval: 10 * time.Millisecond},
		func(ctx context.Context, _ int) error {
			return lb.Do(ctx, func(ctx context.Context, g *harness.Gateway) error {
				_, err := orders.EchoVia(ctx, g, "v1", "rollback")
				return err
			})
		})
	load.WaitOK(t, 100, 30*time.Second)

	running := []string{"b1", "b2", "b3"}
	for _, g := range green {
		cluster.StartNode(g)
		running = append(running, g.Name)
		cluster.WaitNodes(t, running, 60*time.Second)
		lb.WaitInRotation(t, g, 60*time.Second)
	}
	if v, err := green[0].PeersView(ctx); err != nil || v.ActiveLevel != 0 {
		t.Fatalf("level with v2.11.0 peers: %+v %v", v, err)
	}
	// Roll back: g2 is forgotten through g1; g1, the last node that
	// can remove a meta member, leaves by itself.
	g1, g2 := green[0], green[1]
	for _, step := range []struct {
		g     *harness.Gateway
		leave bool
	}{{g2, false}, {g1, true}} {
		id := step.g.NodeID(t)
		lb.Pull(step.g)
		lb.WaitIdle(t, step.g, 10*time.Second)
		ev := g1.Evacuate(t, id, step.leave, 2*time.Minute)
		s13RequireOff(t, js, step.g.Name)
		step.g.Proc.Stop(30 * time.Second)
		if !step.leave {
			if f := g1.Forget(t, id, 2*time.Minute); !f.MetaRemoved {
				t.Fatalf("forget %s: %+v", step.g.Name, f)
			}
		} else if !ev.Left {
			t.Fatalf("%s did not leave: %+v", step.g.Name, ev)
		}
		running = running[:len(running)-1]
	}
	cluster.WaitNodes(t, running, 90*time.Second)

	okNow, _ := load.Counts()
	load.WaitOK(t, okNow+100, 30*time.Second)
	result := load.Stop()
	t.Logf("load: ok=%d failed=%d errors=%v; LB %s", result.OK, result.Failed, result.Errors, lb.StatsString())
	result.RequireNoFailures(t)

	// The blue cluster is as before: level never raised, stable marks,
	// streams on blue only, and the meta group takes a write.
	if rec := s13Bucket(t, js, "gwag-cluster"); rec["level"] == "" || !strings.Contains(rec["level"], `"level":0`) {
		t.Fatalf("cluster level record: %v", rec)
	}
	if after := s13Bucket(t, js, "gwag-stable"); !reflect.DeepEqual(after, stableBefore) {
		t.Fatalf("stable marks changed: before %v after %v", stableBefore, after)
	}
	for name, nodes := range s13Placement(t, js) {
		for _, n := range nodes {
			if !strings.HasPrefix(n, "b") {
				t.Errorf("%s has a replica on %s: %v", name, n, nodes)
			}
		}
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := js.CreateStream(c, jetstream.StreamConfig{Name: "S13B_PROBE", Subjects: []string{"s13b.probe"}, Replicas: 3}); err != nil {
		t.Fatalf("meta write on the blue cluster after rollback: %v", err)
	}
	_ = js.DeleteStream(c, "S13B_PROBE")
	// A second registration on a blue node still replicates.
	billing := env.Fixture(blue[1], harness.FixtureOpts{Namespace: "billing", NoWait: true})
	billing.Proc.WaitLog("FIXTURE ready", 30*time.Second)
	for _, b := range blue {
		b := b
		harness.Eventually(t, 30*time.Second, b.Name+" routes billing", func() error {
			_, err := billing.EchoVia(ctx, b, "v1", "after")
			return err
		})
	}
}

// s13JetStream is a JetStream client whose connection fails over
// between the given NATS URLs, so it outlives any one node.
func s13JetStream(t *testing.T, urls []string) jetstream.JetStream {
	t.Helper()
	nc, err := nats.Connect(strings.Join(urls, ","), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("connect %v: %v", urls, err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	return js
}

// s13Bucket returns a KV bucket's keys and values, read over JetStream.
func s13Bucket(t *testing.T, js jetstream.JetStream, bucket string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kv, err := js.KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("bucket %s: %v", bucket, err)
	}
	out := map[string]string{}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		t.Fatalf("bucket %s keys: %v", bucket, err)
	}
	for _, k := range keys {
		e, err := kv.Get(ctx, k)
		if err != nil {
			t.Fatalf("bucket %s key %s: %v", bucket, k, err)
		}
		out[k] = string(e.Value())
	}
	return out
}

func s13Info(t *testing.T, js jetstream.JetStream, name string) *jetstream.StreamInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatalf("stream %s: %v", name, err)
	}
	return s.CachedInfo()
}

func s13Replicas(t *testing.T, js jetstream.JetStream, name string) int {
	return s13Info(t, js, name).Config.Replicas
}

func s13MarkerTTL(t *testing.T, js jetstream.JetStream, name string) time.Duration {
	return s13Info(t, js, name).Config.SubjectDeleteMarkerTTL
}

// s13Placement maps every stream to the servers holding a replica.
func s13Placement(t *testing.T, js jetstream.JetStream) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out := map[string][]string{}
	l := js.ListStreams(ctx)
	for info := range l.Info() {
		var nodes []string
		if c := info.Cluster; c != nil {
			if c.Leader != "" {
				nodes = append(nodes, c.Leader)
			}
			for _, r := range c.Replicas {
				nodes = append(nodes, r.Name)
			}
		}
		sort.Strings(nodes)
		out[info.Config.Name] = nodes
	}
	if err := l.Err(); err != nil {
		t.Fatalf("list streams: %v", err)
	}
	return out
}

func s13LogPlacement(t *testing.T, js jetstream.JetStream, label string) {
	t.Helper()
	p := s13Placement(t, js)
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		i := s13Info(t, js, n)
		desired := ""
		if i.Cluster != nil && i.Cluster.Desired != nil {
			desired = fmt.Sprintf(" desired=%+v", *i.Cluster.Desired)
		}
		t.Logf("%s: %s R%d leader=%s on %v%s", label, n, i.Config.Replicas, i.Cluster.Leader, p[n], desired)
	}
}

// s13RequireOff fails if any stream still has a replica on node.
func s13RequireOff(t *testing.T, js jetstream.JetStream, node string) {
	t.Helper()
	for name, nodes := range s13Placement(t, js) {
		for _, n := range nodes {
			if n == node {
				t.Fatalf("%s still has a replica on %s after evacuation: %v", name, node, nodes)
			}
		}
	}
}

// s13RequireR3Off fails unless every stream (gateway buckets, the
// event stream) is at R3 on three servers, none of them in gone.
// After the last evacuation that is the green nodes only.
func s13RequireR3Off(t *testing.T, js jetstream.JetStream, gone []string) {
	t.Helper()
	for name, nodes := range s13Placement(t, js) {
		if r := s13Replicas(t, js, name); r != 3 || len(nodes) != 3 {
			t.Fatalf("%s at R%d on %v after evacuating %v", name, r, nodes, gone)
		}
		for _, n := range nodes {
			if slices.Contains(gone, n) {
				t.Fatalf("%s still has a replica on %s after evacuation: %v", name, n, nodes)
			}
		}
	}
}

// s13Publisher publishes numbered events into s13Stream, each with a
// message id and retried until acked, over a connection that fails
// over between the nodes' NATS ports.
type s13Publisher struct {
	acked   atomic.Int64
	retries atomic.Int64
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func s13StartPublisher(t *testing.T, urls []string) *s13Publisher {
	t.Helper()
	nc, err := nats.Connect(strings.Join(urls, ","), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &s13Publisher{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for n := 1; ctx.Err() == nil; n++ {
			body, _ := json.Marshal(map[string]int{"n": n})
			for attempt := 0; ; attempt++ {
				c, cc := context.WithTimeout(ctx, 3*time.Second)
				ack, err := js.Publish(c, fmt.Sprintf("swap.ticks.%d", n), body, jetstream.WithMsgID(fmt.Sprintf("s13-%d", n)))
				cc()
				if err == nil {
					// A duplicate after a retry is the store of an
					// earlier try whose ack was lost. On the first
					// try there is no earlier store.
					if ack.Duplicate && attempt == 0 {
						p.mu.Lock()
						p.err = fmt.Errorf("event %d: duplicate ack on first store", n)
						p.mu.Unlock()
					}
					p.acked.Store(int64(n))
					break
				}
				if ctx.Err() != nil {
					return
				}
				p.retries.Add(1)
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return p
}

func (p *s13Publisher) waitAcked(t *testing.T, n int64, timeout time.Duration) {
	t.Helper()
	harness.Eventually(t, timeout, fmt.Sprintf("%d events acked", n), func() error {
		if a := p.acked.Load(); a < n {
			return fmt.Errorf("%d acked", a)
		}
		return nil
	})
}

// stop ends publishing and returns the count acked. An event whose
// publish was cut by stop is not counted and not retried, so it may
// or may not be stored; the caller compares after the last ack, and
// stop waits for the loop to end first.
func (p *s13Publisher) stop(t *testing.T) int64 {
	t.Helper()
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		t.Fatal(p.err)
	}
	return p.acked.Load()
}
