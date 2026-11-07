//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ErrNoBackend is what LB.Do returns when no node is in rotation.
var ErrNoBackend = errors.New("lb: no healthy backend")

// LB spreads calls over the nodes of a cluster the way a load balancer
// with health checks would:
//
//   - a node is in rotation while its last /api/health probe answered
//     200 and it has not been pulled by hand;
//   - /api/health answers 503 while a node drains (SIGTERM) and
//     refuses connections while it is down or still starting, so both
//     take the node out within one probe interval;
//   - Pull / Restore model deregistration: a pulled node gets no new
//     calls, and WaitIdle waits for its in-flight calls (connection
//     draining) before the operator signals the process.
//
// Calls already sent to a node when it dies fail. LB counts outcomes
// per node so a story can tell "failed on the node that was killed"
// from "failed on a node that stayed up".
type LB struct {
	nodes []*lbNode
	rr    atomic.Uint64
	stop  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
}

type lbNode struct {
	gw       *Gateway
	probeOK  atomic.Bool
	pulled   atomic.Bool
	inflight atomic.Int64
	sent     atomic.Int64
	ok       atomic.Int64
	failed   atomic.Int64
}

// LBOpts configures an LB.
type LBOpts struct {
	// ProbeInterval is the health-check period. Default 100ms.
	ProbeInterval time.Duration
}

// LB starts a load balancer over the cluster's current nodes. It
// probes once before it returns, so nodes that are up are in
// rotation. It stops at test end.
func (c *Cluster) LB(t testing.TB, opts LBOpts) *LB {
	t.Helper()
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = 100 * time.Millisecond
	}
	lb := &LB{stop: make(chan struct{})}
	for _, g := range c.Nodes {
		lb.nodes = append(lb.nodes, &lbNode{gw: g})
	}
	client := &http.Client{Timeout: time.Second}
	probe := func() {
		for _, n := range lb.nodes {
			n.probeOK.Store(healthOK(client, n.gw))
		}
	}
	probe()
	lb.wg.Add(1)
	go func() {
		defer lb.wg.Done()
		tick := time.NewTicker(opts.ProbeInterval)
		defer tick.Stop()
		for {
			select {
			case <-lb.stop:
				return
			case <-tick.C:
				probe()
			}
		}
	}()
	t.Cleanup(lb.Close)
	return lb
}

// Close stops the health checker. Safe to call twice.
func (lb *LB) Close() {
	lb.once.Do(func() { close(lb.stop) })
	lb.wg.Wait()
}

func healthOK(c *http.Client, g *Gateway) bool {
	resp, err := c.Get(g.BaseURL() + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (lb *LB) node(g *Gateway) *lbNode {
	for _, n := range lb.nodes {
		if n.gw == g {
			return n
		}
	}
	panic("harness: LB: gateway " + g.Name + " is not a backend")
}

// InRotation reports whether g would get a call now.
func (lb *LB) InRotation(g *Gateway) bool {
	n := lb.node(g)
	return n.probeOK.Load() && !n.pulled.Load()
}

// Pick returns the next node in rotation, round robin, and a done
// func the caller must call with the call's outcome. nil = no node.
func (lb *LB) Pick() (*Gateway, func(error)) {
	start := int(lb.rr.Add(1))
	for i := 0; i < len(lb.nodes); i++ {
		n := lb.nodes[(start+i)%len(lb.nodes)]
		if !n.probeOK.Load() || n.pulled.Load() {
			continue
		}
		n.inflight.Add(1)
		n.sent.Add(1)
		return n.gw, func(err error) {
			if err != nil {
				n.failed.Add(1)
			} else {
				n.ok.Add(1)
			}
			n.inflight.Add(-1)
		}
	}
	return nil, nil
}

// Do sends one call to the next node in rotation. fn gets the node;
// its error counts against that node.
func (lb *LB) Do(ctx context.Context, fn func(ctx context.Context, gw *Gateway) error) error {
	g, done := lb.Pick()
	if g == nil {
		return ErrNoBackend
	}
	err := fn(ctx, g)
	done(err)
	return err
}

// Pull takes g out of rotation without touching the process.
func (lb *LB) Pull(g *Gateway) { lb.node(g).pulled.Store(true) }

// Restore puts a pulled node back; it still needs a passing probe.
func (lb *LB) Restore(g *Gateway) { lb.node(g).pulled.Store(false) }

// WaitIdle waits until g has no call in flight.
func (lb *LB) WaitIdle(t testing.TB, g *Gateway, timeout time.Duration) {
	t.Helper()
	n := lb.node(g)
	Eventually(t, timeout, g.Name+" has no call in flight", func() error {
		if v := n.inflight.Load(); v != 0 {
			return fmt.Errorf("%d in flight", v)
		}
		return nil
	})
}

// WaitInRotation waits until g passes a health probe and is not pulled.
func (lb *LB) WaitInRotation(t testing.TB, g *Gateway, timeout time.Duration) {
	t.Helper()
	Eventually(t, timeout, g.Name+" is in rotation", func() error {
		if !lb.InRotation(g) {
			return errors.New("not in rotation")
		}
		return nil
	})
}

// WaitOutOfRotation waits until g fails its health probe (or is pulled).
func (lb *LB) WaitOutOfRotation(t testing.TB, g *Gateway, timeout time.Duration) {
	t.Helper()
	Eventually(t, timeout, g.Name+" is out of rotation", func() error {
		if lb.InRotation(g) {
			return errors.New("still in rotation")
		}
		return nil
	})
}

// NodeStats is one node's call counts.
type NodeStats struct{ Sent, OK, Failed int64 }

// Stats returns per-node call counts since the LB started.
func (lb *LB) Stats() map[string]NodeStats {
	out := map[string]NodeStats{}
	for _, n := range lb.nodes {
		out[n.gw.Name] = NodeStats{Sent: n.sent.Load(), OK: n.ok.Load(), Failed: n.failed.Load()}
	}
	return out
}

// StatsString summarises Stats for logs.
func (lb *LB) StatsString() string {
	st := lb.Stats()
	names := make([]string, 0, len(st))
	for n := range st {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		s := st[n]
		parts[i] = fmt.Sprintf("%s sent=%d ok=%d failed=%d", n, s.Sent, s.OK, s.Failed)
	}
	return strings.Join(parts, "; ")
}

// RawGet fetches path from the node and returns the body and status
// (no status check, no decoding).
func (g *Gateway) RawGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL()+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// PeerCount is the number of peers the node lists in /api/admin/peers
// (itself included).
func (g *Gateway) PeerCount(ctx context.Context) (int, error) {
	var out struct {
		Peers []struct {
			NodeID string `json:"nodeId"`
		} `json:"peers"`
	}
	if err := g.Get(ctx, "/api/admin/peers", &out); err != nil {
		return 0, err
	}
	return len(out.Peers), nil
}

// PeerNames is the sorted, distinct node names in /api/admin/peers. A
// node that restarts gets a new node id and its old peer entry stays
// until the 30s peer TTL, so the entry count can exceed the node
// count for a while; the name set does not.
func (g *Gateway) PeerNames(ctx context.Context) ([]string, error) {
	var out struct {
		Peers []struct {
			Name string `json:"name"`
		} `json:"peers"`
	}
	if err := g.Get(ctx, "/api/admin/peers", &out); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, p := range out.Peers {
		if !seen[p.Name] {
			seen[p.Name] = true
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// WaitNodes waits until every running node lists exactly the node
// names in want.
func (c *Cluster) WaitNodes(t testing.TB, want []string, timeout time.Duration) {
	t.Helper()
	want = append([]string(nil), want...)
	sort.Strings(want)
	Eventually(t, timeout, fmt.Sprintf("every live node lists peers %v", want), func() error {
		for _, g := range c.Live() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			got, err := g.PeerNames(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: %w", g.Name, err)
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				return fmt.Errorf("%s lists %v", g.Name, got)
			}
		}
		return nil
	})
}

// Names returns the names of all nodes.
func (c *Cluster) Names() []string {
	var out []string
	for _, g := range c.Nodes {
		out = append(out, g.Name)
	}
	return out
}
