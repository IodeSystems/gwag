//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
	"github.com/iodesystems/gwag/v2/events"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

// TestS14FailureModes measures what keeps working, what fails, and
// how long recovery takes, for the failures an operator plans for. A
// 3-node cluster sits behind a round-robin LB that probes
// /api/health. Behind it: catalog (proto, two instances), ledger
// (OpenAPI, one instance), orders (declares the typed event channel
// OrderShipped, one JetStream stream), a ps_sub WebSocket client that
// reconnects through the LB, a ps.pub publisher, and an event
// publisher and group listener over NATS. Traffic runs through every
// scenario; each logs one "S14 |" line per traffic kind (the source of
// the table in docs/running.md).
//
// Scenarios, in order (each ends converged):
//
//	d  the only instance of ledger is killed, then restarted
//	e  one of the two catalog instances is killed, then restarted
//	a  1 of 3 nodes is killed (SIGKILL), then restarted
//	b  2 of 3 nodes are killed: no JetStream quorum. The survivor
//	   keeps routing; writes fail. Then both are restarted.
//
// (c, the LB itself down, is not run: nothing reaches the cluster. f,
// a standalone node, is TestS14bStandaloneRestart.)
func TestS14FailureModes(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Environment: "e2e", Args: []string{"--subscribe-secret", s8Secret}})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]
	ctx := harness.Ctx(t, 15*time.Minute)

	catalog1 := env.Fixture(n1, harness.FixtureOpts{Namespace: "catalog", Instance: "catalog-1", FailoverGateways: []*harness.Gateway{n2, n3}})
	catalog2 := env.Fixture(n2, harness.FixtureOpts{Namespace: "catalog", Instance: "catalog-2", FailoverGateways: []*harness.Gateway{n3, n1}})
	ledger := env.Fixture(n3, harness.FixtureOpts{Kind: harness.KindOpenAPI, Namespace: "ledger", Instance: "ledger-1", FailoverGateways: []*harness.Gateway{n1, n2}})

	root, err := harness.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	ordersReg := s11Register(t, ctx, controlclient.Options{
		GatewayAddr:  n1.CPAddr(),
		GatewayAddrs: []string{n2.CPAddr(), n3.CPAddr()},
		ServiceAddr:  s11GRPCServer(t),
		InstanceID:   "orders-1",
		TTL:          30 * time.Second, // the default, not s11Register's short one
		Services: []controlclient.Service{{
			Namespace: "orders", Version: "v1",
			ProtoSource:  s11Read(t, root, "examples/events/proto/orders/v1/orders.proto"),
			ProtoImports: map[string][]byte{"gw/proto/ps/v1/options.proto": s11Read(t, root, "gw/proto/ps/v1/options.proto")},
		}},
	})
	s11WaitReg(t, ordersReg, "orders-1", 30*time.Second)

	for _, g := range cluster.Nodes {
		for _, f := range []*harness.Fixture{catalog1, catalog2, ledger} {
			g.Admin().WaitInstance(f.Opts.Namespace, "v1", f.Opts.Instance, "ready", 30*time.Second)
		}
		g.Admin().WaitInstance("orders", "v1", "orders-1", "ready", 30*time.Second)
	}
	cluster.WaitStreamsReplicated(t, 3, 75*time.Second)
	cluster.WaitStreamReplicas(t, 3, 30*time.Second, "EV_orders_v1_OrderShipped")

	lb := cluster.LB(t, harness.LBOpts{})
	tr := s14StartTraffic(t, ctx, cluster, lb, catalog1, ledger)
	tr.waitHealthy(t, 60*time.Second)

	t.Run("d only instance of a service killed", func(t *testing.T) {
		ledger.Proc.Kill()
		t0 := time.Now()
		gl := tr.series("graphql ledger")
		// While the registration lives (30s TTL), calls reach the
		// gateway's dispatcher and fail on the dead address.
		harness.Eventually(t, 10*time.Second, "ledger calls fail", func() error {
			if w := gl.window(t0, time.Now()); w.Failed == 0 {
				return errors.New("no failed ledger call yet")
			}
			return nil
		})
		// Then the registration expires on every node and ledger
		// leaves the schema.
		harness.Eventually(t, 75*time.Second, "every node drops ledger", func() error {
			for _, g := range cluster.Nodes {
				ss, err := g.Admin().Services(ctx)
				if err != nil {
					return err
				}
				if ss.Service("ledger", "v1") != nil {
					return fmt.Errorf("%s lists ledger", g.Name)
				}
			}
			return nil
		})
		goneAt := time.Now()
		t.Logf("ledger left every node's registry %s after SIGKILL", goneAt.Sub(t0).Round(time.Millisecond))
		harness.Eventually(t, 10*time.Second, "a ledger call fails after the expiry", func() error {
			if w := gl.window(goneAt, time.Now()); w.Failed == 0 {
				return errors.New("no failed ledger call yet")
			}
			return nil
		})
		t.Logf("error while registered: %s", gl.firstErr(t0, goneAt))
		t.Logf("error after expiry: %s", gl.firstErr(goneAt, time.Now()))

		ledger.Proc.Restart()
		restartAt := time.Now()
		ledger.WaitReady()
		end := tr.waitHealthy(t, 60*time.Second)
		t.Logf("ledger answers again %s after its restart", gl.lastFailEnd(restartAt).Sub(restartAt).Round(time.Millisecond))
		tr.report(t, "d", t0, restartAt, end)
		// Other services do not notice.
		tr.requireNoFailures(t, t0, end, nil, "graphql catalog", "rest catalog", "grpc catalog", "ps.pub", "event publish")
	})

	t.Run("e one of two instances killed", func(t *testing.T) {
		catalog1.Proc.Kill()
		t0 := time.Now()
		harness.Eventually(t, 75*time.Second, "every node drops catalog-1", func() error {
			for _, g := range cluster.Nodes {
				ss, err := g.Admin().Services(ctx)
				if err != nil {
					return err
				}
				if svc := ss.Service("catalog", "v1"); svc == nil || svc.Instance("catalog-1") != nil {
					return fmt.Errorf("%s: catalog %v", g.Name, svc)
				}
			}
			return nil
		})
		t.Logf("catalog-1 left every node's registry %s after SIGKILL", time.Since(t0).Round(time.Millisecond))
		catalog1.Proc.Restart()
		restartAt := time.Now()
		catalog1.WaitReady()
		for _, g := range cluster.Nodes {
			g.Admin().WaitInstance("catalog", "v1", "catalog-1", "ready", 30*time.Second)
		}
		end := tr.waitHealthy(t, 60*time.Second)
		tr.report(t, "e", t0, restartAt, end)
		// Calls in flight on catalog-1 at the kill are not retried
		// (the request was sent); a call refused at connect is.
		tr.requireFewFailures(t, t0, end, 2, "graphql catalog", "rest catalog", "grpc catalog")
		tr.requireNoFailures(t, t0, end, nil, "graphql ledger", "ps.pub", "event publish")
	})

	t.Run("a one of three nodes killed", func(t *testing.T) {
		cluster.WaitStreamsReplicated(t, 3, 75*time.Second)
		n2.Proc.Kill()
		t0 := time.Now()
		lb.WaitOutOfRotation(t, n2, 5*time.Second)
		t.Logf("n2 left the LB rotation %s after SIGKILL", time.Since(t0).Round(time.Millisecond))
		harness.Eventually(t, 75*time.Second, "survivors list 2 peers", func() error {
			for _, g := range []*harness.Gateway{n1, n3} {
				if n, err := g.PeerCount(ctx); err != nil || n != 2 {
					return fmt.Errorf("%s lists %d peers (%v)", g.Name, n, err)
				}
			}
			return nil
		})
		// Registrations held by n2 moved; their old entries expired.
		harness.Eventually(t, 75*time.Second, "survivors list each instance once", func() error {
			return s14OneEach(ctx, []*harness.Gateway{n1, n3})
		})
		t.Logf("survivors list one registration per instance %s after SIGKILL", time.Since(t0).Round(time.Millisecond))
		n2.Proc.Restart()
		restartAt := time.Now()
		lb.WaitInRotation(t, n2, 60*time.Second)
		t.Logf("n2 back in LB rotation %s after restart", time.Since(restartAt).Round(time.Millisecond))
		n2.WaitReady()
		cluster.WaitNodes(t, cluster.Names(), 60*time.Second)
		end := tr.waitHealthy(t, 60*time.Second)
		tr.report(t, "a", t0, restartAt, end)
		// Calls sent to n2 before the LB saw it down fail; none sent
		// to a live node does, and none sent to n2 after its restart.
		tr.requireNoFailures(t, t0, end, map[string]bool{n2.Name: true}, "graphql catalog", "graphql ledger", "rest catalog", "grpc catalog")
		tr.requireNoFailuresOn(t, restartAt, end, n2.Name, "graphql catalog", "graphql ledger", "rest catalog", "grpc catalog")
		cluster.WaitStreamsReplicated(t, 3, 75*time.Second)
	})

	t.Run("b two of three nodes killed", func(t *testing.T) {
		n2.Proc.Kill()
		n3.Proc.Kill()
		t0 := time.Now()
		lb.WaitOutOfRotation(t, n2, 5*time.Second)
		lb.WaitOutOfRotation(t, n3, 5*time.Second)

		harness.Eventually(t, 30*time.Second, "n1 health reports jetstream unavailable", func() error {
			body, status, err := n1.Health(ctx)
			if err != nil {
				return err
			}
			if status != http.StatusOK || body["status"] != "serving" {
				return harness.Permanent(fmt.Errorf("n1 health %d %v, want 200 serving", status, body))
			}
			if body["jetstream"] != "unavailable" {
				return fmt.Errorf("health %v", body)
			}
			return nil
		})
		t.Logf("n1 health: 200 serving, jetstream unavailable, %s after the kills", time.Since(t0).Round(time.Millisecond))

		// A write: refused while there is no quorum.
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		derr := n1.Admin().Deprecate(wctx, "catalog", "v1", "s14 no quorum")
		cancel()
		if derr == nil {
			t.Errorf("deprecate without a JetStream quorum succeeded, want an error")
		}
		t.Logf("admin write without quorum: %v", derr)
		// A new service cannot register.
		late := env.Fixture(n1, harness.FixtureOpts{Namespace: "late", Instance: "late-1", NoWait: true})

		// Past the registration TTL plus the janitor period: before
		// the fix, n1 dropped every service registered through it
		// here (catalog-1, orders-1) though the instances were up.
		harness.Consistently(t, 45*time.Second, "n1 serves 200 and keeps its registry", func() error {
			body, status, err := n1.Health(ctx)
			if err != nil || status != http.StatusOK || body["status"] != "serving" {
				return fmt.Errorf("n1 health %d %v %v", status, body, err)
			}
			return nil
		})
		ss, err := n1.Admin().Services(ctx)
		if err != nil {
			t.Fatalf("admin read without quorum: %v", err)
		}
		if ss.Service("late", "v1") != nil {
			t.Errorf("late registered without a JetStream quorum")
		}
		for _, id := range []string{"catalog-1", "catalog-2"} {
			if svc := ss.Service("catalog", "v1"); svc == nil || svc.Instance(id) == nil {
				t.Errorf("n1 lost %s from its registry without quorum", id)
			}
		}
		if ss.Service("ledger", "v1") == nil || ss.Service("orders", "v1") == nil {
			t.Errorf("n1 lost ledger or orders from its registry without quorum")
		}

		n2.Proc.Restart()
		n3.Proc.Restart()
		restartAt := time.Now()
		n2.WaitReady()
		n3.WaitReady()
		t.Logf("n2 and n3 serve /api/health 200 %s after restart", time.Since(restartAt).Round(time.Millisecond))
		harness.Eventually(t, 60*time.Second, "n1 health reports jetstream ok", func() error {
			body, _, err := n1.Health(ctx)
			if err != nil {
				return err
			}
			if body["jetstream"] != "ok" {
				return fmt.Errorf("health %v", body)
			}
			return nil
		})
		t.Logf("n1 health: jetstream ok %s after restart", time.Since(restartAt).Round(time.Millisecond))
		late.Proc.WaitLog("FIXTURE ready", 60*time.Second)
		n1.Admin().WaitInstance("late", "v1", "late-1", "ready", 60*time.Second)
		t.Logf("late registered %s after restart", time.Since(restartAt).Round(time.Millisecond))
		cluster.WaitNodes(t, cluster.Names(), 75*time.Second)
		end := tr.waitHealthy(t, 90*time.Second)
		tr.report(t, "b", t0, restartAt, end)
		// The fix: nothing sent to n1 failed while it was alone.
		tr.requireNoFailuresOn(t, t0, restartAt, n1.Name, "graphql catalog", "graphql ledger", "rest catalog", "grpc catalog")
		cluster.WaitStreamsReplicated(t, 3, 75*time.Second)
	})

	tr.stop(t)
	t.Run("every acked event delivered", func(t *testing.T) {
		tr.waitEventsDelivered(t, 90*time.Second)
	})
}

// TestS14bStandaloneRestart: scenario f. One gateway with embedded
// NATS and no peers (standalone JetStream, state on disk), one
// service, a deprecation stored in KV. SIGKILL the gateway: every
// call fails. Restart it on the same data directory: it routes the
// service from the registry on disk at once, the deprecation is still
// there, the next heartbeat registers the service again and the old
// entry expires, with no failed call after the first ok one.
func TestS14bStandaloneRestart(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Name: "solo", NATS: true})
	catalog := env.Fixture(gw, harness.FixtureOpts{Namespace: "catalog"})
	ctx := harness.Ctx(t, 5*time.Minute)
	const reason = "s14 kept on disk"
	if err := gw.Admin().Deprecate(ctx, "catalog", "v1", reason); err != nil {
		t.Fatal(err)
	}

	s := &s14Series{name: "graphql catalog"}
	load := harness.StartLoad(t, harness.LoadOpts{Workers: 2, Interval: 20 * time.Millisecond, CallTimeout: 5 * time.Second},
		func(ctx context.Context, _ int) error {
			_, err := catalog.Echo(ctx, "v1", "load")
			s.add(gw.Name, err)
			return err
		})
	load.WaitOK(t, 50, 20*time.Second)

	gw.Proc.Kill()
	t0 := time.Now()
	harness.Eventually(t, 5*time.Second, "calls fail", func() error {
		if s.window(t0, time.Now()).Failed == 0 {
			return errors.New("no failure yet")
		}
		return nil
	})
	gw.Proc.Restart()
	restartAt := time.Now()
	gw.WaitReady()
	readyAt := time.Now()
	harness.Eventually(t, 60*time.Second, "catalog answers again", func() error {
		w := s.window(readyAt, time.Now())
		if w.OK < 20 || time.Since(w.LastFail) < time.Second {
			return fmt.Errorf("since ready: %+v", w)
		}
		return nil
	})
	firstOK := s.firstOK(restartAt)

	// The registry is on disk: the restarted node replays the old
	// entry and routes catalog before the service has said anything.
	// Its control plane does not know the registration, so the next
	// heartbeat registers again; the old entry expires one TTL after
	// its last refresh. Neither may cost a call.
	ss, err := gw.Admin().Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := ss.Service("catalog", "v1")
	if svc == nil || svc.ManualDeprecationReason != reason {
		t.Errorf("after restart: catalog/v1 %+v, want deprecation %q kept", svc, reason)
	}
	var reReg, expired time.Duration
	harness.Eventually(t, 30*time.Second, "catalog registers again", func() error {
		regs, err := gw.Admin().Registrations(ctx)
		if err != nil || len(regs) != 1 {
			return fmt.Errorf("%d registrations (%v)", len(regs), err)
		}
		reReg = time.Since(restartAt)
		return nil
	})
	regs, _ := gw.Admin().Registrations(ctx)
	newID := regs[0].GetRegistrationId()
	harness.Eventually(t, 75*time.Second, "the old registry entry expires", func() error {
		ss, err := gw.Admin().Services(ctx)
		if err != nil {
			return err
		}
		svc := ss.Service("catalog", "v1")
		if svc == nil || len(svc.Instances) != 1 || svc.Instances[0].RegistrationID != newID {
			return fmt.Errorf("catalog %+v", svc)
		}
		expired = time.Since(restartAt)
		return nil
	})
	// A few calls after the expiry.
	harness.Eventually(t, 10*time.Second, "calls after the expiry", func() error {
		if w := s.window(restartAt.Add(expired), time.Now()); w.OK < 20 {
			return fmt.Errorf("%+v", w)
		}
		return nil
	})
	res := load.Stop()
	t.Logf("S14 | f | graphql catalog | ok=%d failed=%d | failing from the kill until +%s after restart (restart to /api/health 200: %s) | registered again %s after restart, old entry gone %s after restart",
		res.OK, res.Failed, firstOK.Sub(restartAt).Round(time.Millisecond), readyAt.Sub(restartAt).Round(time.Millisecond),
		reReg.Round(time.Millisecond), expired.Round(time.Millisecond))
	if after := s.window(firstOK, time.Now()); after.Failed != 0 {
		t.Errorf("calls failed after the first ok one: %+v, first: %s", after, s.firstErr(firstOK, time.Now()))
	}
}

// s14OneEach: each of the four instances is listed once (one
// registration) and ready on every node in gs.
func s14OneEach(ctx context.Context, gs []*harness.Gateway) error {
	want := map[string][]string{"catalog": {"catalog-1", "catalog-2"}, "ledger": {"ledger-1"}, "orders": {"orders-1"}}
	for _, g := range gs {
		ss, err := g.Admin().Services(ctx)
		if err != nil {
			return err
		}
		for ns, ids := range want {
			svc := ss.Service(ns, "v1")
			if svc == nil {
				return fmt.Errorf("%s does not list %s", g.Name, ns)
			}
			if len(svc.Instances) != len(ids) {
				return fmt.Errorf("%s lists %d %s instances, want %d", g.Name, len(svc.Instances), ns, len(ids))
			}
			for _, id := range ids {
				if in := svc.Instance(id); in == nil || in.State != "ready" {
					return fmt.Errorf("%s: %s not ready", g.Name, id)
				}
			}
		}
	}
	return nil
}

// ---- traffic ------------------------------------------------------------

// s14Event is one call's outcome.
type s14Event struct {
	at   time.Time
	node string
	err  error
}

// s14Series is the outcome of every call of one traffic kind.
type s14Series struct {
	name string
	mu   sync.Mutex
	evs  []s14Event
}

func (s *s14Series) add(node string, err error) {
	s.mu.Lock()
	s.evs = append(s.evs, s14Event{at: time.Now(), node: node, err: err})
	s.mu.Unlock()
}

type s14Window struct {
	OK, Failed          int
	FirstFail, LastFail time.Time
	FailedOn            map[string]int // node -> failed calls
}

func (s *s14Series) window(from, to time.Time) s14Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s14Window{FailedOn: map[string]int{}}
	for _, e := range s.evs {
		if e.at.Before(from) || e.at.After(to) {
			continue
		}
		if e.err == nil {
			w.OK++
			continue
		}
		w.Failed++
		w.FailedOn[e.node]++
		if w.FirstFail.IsZero() {
			w.FirstFail = e.at
		}
		w.LastFail = e.at
	}
	return w
}

// firstErr is the first error in [from, to], "" if none.
func (s *s14Series) firstErr(from, to time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.evs {
		if e.err != nil && !e.at.Before(from) && !e.at.After(to) {
			return fmt.Sprintf("[%s] %v", e.node, e.err)
		}
	}
	return ""
}

// errors are up to n distinct error messages in [from, to] with counts.
func (s *s14Series) errors(from, to time.Time, n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var order []string
	for _, e := range s.evs {
		if e.err == nil || e.at.Before(from) || e.at.After(to) {
			continue
		}
		msg := e.err.Error()
		if len(msg) > 160 {
			msg = msg[:160]
		}
		if _, ok := counts[msg]; !ok {
			if len(order) >= n {
				continue
			}
			order = append(order, msg)
		}
		counts[msg]++
	}
	var parts []string
	for _, m := range order {
		parts = append(parts, fmt.Sprintf("%dx %q", counts[m], m))
	}
	return strings.Join(parts, "; ")
}

// firstOK is the time of the first ok call at or after from.
func (s *s14Series) firstOK(from time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.evs {
		if e.err == nil && !e.at.Before(from) {
			return e.at
		}
	}
	return time.Time{}
}

// lastFailEnd is the first ok call after the last failure at or after
// from (from itself when nothing failed).
func (s *s14Series) lastFailEnd(from time.Time) time.Time {
	w := s.window(from, time.Now())
	if w.Failed == 0 {
		return from
	}
	return s.firstOK(w.LastFail)
}

// s14Traffic runs every traffic kind until stop.
type s14Traffic struct {
	all  []*s14Series
	stop func(t *testing.T)

	evMu      sync.Mutex
	acked     map[string]time.Time // event id -> publish ack
	delivered map[string]time.Time // event id -> first delivery

	psMu       sync.Mutex
	psSent     map[int]time.Time // ps.pub ok
	psGot      map[int]bool
	psReconn   []string // "+Xs node: why" per subscription end
	psSubStart time.Time
}

func (tr *s14Traffic) series(name string) *s14Series {
	for _, s := range tr.all {
		if s.name == name {
			return s
		}
	}
	panic("s14: no series " + name)
}

func s14StartTraffic(t *testing.T, ctx context.Context, cluster *harness.Cluster, lb *harness.LB, catalog, ledger *harness.Fixture) *s14Traffic {
	t.Helper()
	tr := &s14Traffic{acked: map[string]time.Time{}, delivered: map[string]time.Time{}, psSent: map[int]time.Time{}, psGot: map[int]bool{}}
	viaLB := func(name string, call func(ctx context.Context, g *harness.Gateway) error) func() {
		s := &s14Series{name: name}
		tr.all = append(tr.all, s)
		return func() {
			// Not the load's context: a call cancelled by stop is
			// neither ok nor failed.
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			node := ""
			err := lb.Do(cctx, func(ctx context.Context, g *harness.Gateway) error {
				node = g.Name
				return call(ctx, g)
			})
			s.add(node, err)
		}
	}
	check := func(f *harness.Fixture) func(harness.Echo, error) error {
		return func(e harness.Echo, err error) error {
			if err == nil && (e.Msg != "load" || e.Namespace != f.Opts.Namespace) {
				err = fmt.Errorf("wrong answer %+v", e)
			}
			return err
		}
	}
	calls := []func(){
		viaLB("graphql catalog", func(ctx context.Context, g *harness.Gateway) error {
			return check(catalog)(catalog.EchoVia(ctx, g, "v1", "load"))
		}),
		viaLB("graphql ledger", func(ctx context.Context, g *harness.Gateway) error {
			return check(ledger)(ledger.EchoVia(ctx, g, "v1", "load"))
		}),
		viaLB("rest catalog", func(ctx context.Context, g *harness.Gateway) error {
			return check(catalog)(catalog.EchoRESTVia(ctx, g, "v1", "load"))
		}),
		viaLB("grpc catalog", func(ctx context.Context, g *harness.Gateway) error {
			return check(catalog)(catalog.EchoGRPCVia(ctx, g, "v1", "load", ""))
		}),
	}

	// ps.pub through the LB; ps_sub held by a client that reconnects
	// through the LB when its socket ends.
	const channel = "events.s14.ticks"
	tok, err := cluster.Nodes[0].Sign(ctx, channel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var psSeq atomic.Int64
	calls = append(calls, viaLB("ps.pub", func(ctx context.Context, g *harness.Gateway) error {
		n := int(psSeq.Add(1))
		err := g.Publish(ctx, channel, "seq-"+strconv.Itoa(n), tok)
		if err == nil {
			tr.psMu.Lock()
			tr.psSent[n] = time.Now()
			tr.psMu.Unlock()
		}
		return err
	}))

	// Typed events over NATS, one attempt per id.
	listener := s11Bus(t, cluster.NATS(t))
	if _, err := listener.OnOrderShipped(ctx, "s14", func(ctx context.Context, _ ordersv1.OrderShippedSubject, _ *ordersv1.OrderShipped) error {
		if m := events.MessageFrom(ctx); m != nil {
			tr.evMu.Lock()
			if _, ok := tr.delivered[m.ID]; !ok {
				tr.delivered[m.ID] = time.Now()
			}
			tr.evMu.Unlock()
		}
		return nil
	}, events.AckWait(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	producer := s11Bus(t, cluster.NATS(t))
	evSeries := &s14Series{name: "event publish"}
	tr.all = append(tr.all, evSeries)
	var evSeq atomic.Int64
	calls = append(calls, func() {
		id := fmt.Sprintf("ev-%d", evSeq.Add(1))
		pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := producer.PublishOrderShipped(pctx, ordersv1.OrderShippedSubject{OrderId: id}, &ordersv1.OrderShipped{OrderId: id, Carrier: "ups"}, events.MsgID(id))
		evSeries.add("nats", err)
		if err == nil {
			tr.evMu.Lock()
			tr.acked[id] = time.Now()
			tr.evMu.Unlock()
		}
	})

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range calls {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				c()
				select {
				case <-stopCh:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
	}

	subQuery := fmt.Sprintf(`subscription { ps_sub(channel: %q, %s) { channel payload ts } }`, channel, tok.Args())
	subSeries := &s14Series{name: "ps_sub connect"}
	tr.all = append(tr.all, subSeries)
	tr.psSubStart = time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			g := lb.Next()
			if g == nil {
				subSeries.add("", harness.ErrNoBackend)
				select {
				case <-stopCh:
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			sub, err := g.Subscribe(t, subQuery)
			subSeries.add(g.Name, err)
			if err != nil {
				select {
				case <-stopCh:
					return
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			func() {
				for {
					select {
					case <-stopCh:
						sub.Close()
						return
					case data := <-sub.Events:
						var d struct {
							PsSub struct{ Payload string } `json:"ps_sub"`
						}
						if json.Unmarshal(data, &d) == nil {
							if n, err := strconv.Atoi(strings.TrimPrefix(d.PsSub.Payload, "seq-")); err == nil {
								tr.psMu.Lock()
								tr.psGot[n] = true
								tr.psMu.Unlock()
							}
						}
					case <-sub.Done:
						tr.psMu.Lock()
						tr.psReconn = append(tr.psReconn, fmt.Sprintf("%s %s: %v", time.Now().Format("15:04:05.000"), g.Name, sub.Err()))
						tr.psMu.Unlock()
						return
					}
				}
			}()
		}
	}()

	var once sync.Once
	tr.stop = func(t *testing.T) {
		once.Do(func() {
			close(stopCh)
			wg.Wait()
		})
	}
	t.Cleanup(func() { tr.stop(t) })
	return tr
}

// waitHealthy waits until every series has had ok calls and no
// failure for 2s, and returns that moment (the first ok after the
// last failure is when recovery happened; the 2s is the check).
func (tr *s14Traffic) waitHealthy(t *testing.T, timeout time.Duration) time.Time {
	t.Helper()
	harness.Eventually(t, timeout, "every traffic kind succeeds for 2s", func() error {
		now := time.Now()
		for _, s := range tr.all {
			if s.name == "ps_sub connect" {
				continue // one call per (re)connect
			}
			w := s.window(now.Add(-2*time.Second), now)
			if w.Failed > 0 || w.OK == 0 {
				return fmt.Errorf("%s: ok=%d failed=%d in the last 2s", s.name, w.OK, w.Failed)
			}
		}
		return nil
	})
	return time.Now()
}

// report logs one line per series for scenario: calls between t0
// (the fault) and end, the failing span relative to t0, and the first
// error messages. restartAt is when the fault was undone.
func (tr *s14Traffic) report(t *testing.T, scenario string, t0, restartAt, end time.Time) {
	t.Helper()
	rel := func(x time.Time) string { return fmt.Sprintf("+%.1fs", x.Sub(t0).Seconds()) }
	t.Logf("S14 | %s | fault at +0.0s, undone at %s, healthy check at %s", scenario, rel(restartAt), rel(end))
	for _, s := range tr.all {
		w := s.window(t0, end)
		span := "no failures"
		if w.Failed > 0 {
			var nodes []string
			for n, c := range w.FailedOn {
				nodes = append(nodes, fmt.Sprintf("%s=%d", n, c))
			}
			sort.Strings(nodes)
			span = fmt.Sprintf("failing %s..%s, ok again at %s (%s; %d after the undo)", rel(w.FirstFail), rel(w.LastFail), rel(s.firstOK(w.LastFail)), strings.Join(nodes, " "), s.window(restartAt, end).Failed)
		}
		t.Logf("S14 | %s | %-15s | ok=%d failed=%d | %s | %s", scenario, s.name, w.OK, w.Failed, span, s.errors(t0, end, 3))
	}
	tr.psMu.Lock()
	lost := 0
	for n, at := range tr.psSent {
		if !at.Before(t0) && !at.After(end.Add(-2*time.Second)) && !tr.psGot[n] {
			lost++
		}
	}
	ends := tr.psReconn
	tr.psReconn = nil
	tr.psMu.Unlock()
	t.Logf("S14 | %s | ps_sub delivery | %d acked ps.pub events not received; subscription ends: %v", scenario, lost, ends)
	tr.evMu.Lock()
	pending := 0
	for id, at := range tr.acked {
		if !at.Before(t0) && !at.After(end) {
			if _, ok := tr.delivered[id]; !ok {
				pending++
			}
		}
	}
	tr.evMu.Unlock()
	t.Logf("S14 | %s | event delivery  | %d acked events not yet delivered at the healthy check", scenario, pending)
}

// requireNoFailures fails the test if a series in names failed in
// [from, to] on a node not in except.
func (tr *s14Traffic) requireNoFailures(t *testing.T, from, to time.Time, except map[string]bool, names ...string) {
	t.Helper()
	for _, name := range names {
		w := tr.series(name).window(from, to)
		for node, n := range w.FailedOn {
			if !except[node] {
				t.Errorf("%s: %d failed calls on %s; first: %s", name, n, node, tr.series(name).firstErr(from, to))
			}
		}
	}
}

// requireNoFailuresOn fails the test if a series in names failed on
// node in [from, to].
func (tr *s14Traffic) requireNoFailuresOn(t *testing.T, from, to time.Time, node string, names ...string) {
	t.Helper()
	for _, name := range names {
		if n := tr.series(name).window(from, to).FailedOn[node]; n != 0 {
			t.Errorf("%s: %d failed calls on %s; first: %s", name, n, node, tr.series(name).firstErr(from, to))
		}
	}
}

// requireFewFailures fails the test if a series in names failed more
// than max times in [from, to].
func (tr *s14Traffic) requireFewFailures(t *testing.T, from, to time.Time, max int, names ...string) {
	t.Helper()
	for _, name := range names {
		if w := tr.series(name).window(from, to); w.Failed > max {
			t.Errorf("%s: %d failed calls, want at most %d; %s", name, w.Failed, max, tr.series(name).errors(from, to, 3))
		}
	}
}

// waitEventsDelivered waits until every acked event reached the
// listener group at least once.
func (tr *s14Traffic) waitEventsDelivered(t *testing.T, timeout time.Duration) {
	t.Helper()
	harness.Eventually(t, timeout, "every acked event delivered", func() error {
		tr.evMu.Lock()
		defer tr.evMu.Unlock()
		var missing []string
		for id := range tr.acked {
			if _, ok := tr.delivered[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%d of %d acked events not delivered: %v", len(missing), len(tr.acked), s11Head(missing, 10))
		}
		return nil
	})
	tr.evMu.Lock()
	t.Logf("events: %d acked, all delivered", len(tr.acked))
	tr.evMu.Unlock()
}
