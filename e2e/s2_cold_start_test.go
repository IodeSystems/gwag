//go:build e2e

package e2e

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// fleet is the service DAG of the cold-start story. An edge a -> b
// means a depends on b:v1.
//
//	checkout -> orders, auth
//	orders   -> auth, catalog
//	auth     -> db
//	catalog  -> db
var fleet = map[string][]string{
	"db":       nil,
	"auth":     {"db"},
	"catalog":  {"db"},
	"orders":   {"auth", "catalog"},
	"checkout": {"orders", "auth"},
}

// readySet is the fixpoint of the start gate: a started service is
// ready when every dependency is started and ready.
func readySet(started []string) map[string]bool {
	in := map[string]bool{}
	for _, s := range started {
		in[s] = true
	}
	ready := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, s := range started {
			if ready[s] {
				continue
			}
			ok := true
			for _, d := range fleet[s] {
				if !ready[d] {
					ok = false
				}
			}
			if ok {
				ready[s] = true
				changed = true
			}
		}
	}
	return ready
}

// shuffledStartOrder returns the fleet in a seeded random order with
// at least two services that start before their dependencies are
// ready. The seed comes from the test name and E2E_SEED (default: the
// clock); both are logged so a failure can be replayed with
// E2E_SEED=<n>.
func shuffledStartOrder(t *testing.T) []string {
	t.Helper()
	base := time.Now().UnixNano() % 1_000_000
	if v := os.Getenv("E2E_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("E2E_SEED %q: %v", v, err)
		}
		base = n
	}
	h := fnv.New64a()
	h.Write([]byte(t.Name()))
	seed := int64(h.Sum64()>>1) + base
	t.Logf("shuffle seed: name=%s E2E_SEED=%d (rerun with E2E_SEED=%d)", t.Name(), base, base)

	names := make([]string, 0, len(fleet))
	for n := range fleet {
		names = append(names, n)
	}
	sort.Strings(names)
	rng := rand.New(rand.NewSource(seed))
	for {
		order := append([]string(nil), names...)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		if len(lateStarters(order)) >= 2 {
			t.Logf("start order: %v", order)
			return order
		}
	}
}

// lateStarters lists the services that register while a dependency is
// not ready yet, in the given start order.
func lateStarters(order []string) []string {
	var late []string
	for i, s := range order {
		ready := readySet(order[:i])
		for _, d := range fleet[s] {
			if !ready[d] {
				late = append(late, s)
				break
			}
		}
	}
	return late
}

// TestS2ColdFleetStart starts five services with a dependency DAG in a
// shuffled order, against a gateway that knows none of them. A service
// waits until its dependencies are ready and the fleet converges to
// ready. Then a registration that would close a cycle is refused.
func TestS2ColdFleetStart(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	admin := gw.Admin()
	ctx := harness.Ctx(t, 3*time.Minute)
	order := shuffledStartOrder(t)
	late := lateStarters(order)
	isLate := map[string]bool{}
	for _, s := range late {
		isLate[s] = true
	}
	t.Logf("late starters: %v", late)

	const wait = 30 * time.Second
	before := gw.MustScrape(t)

	fixtures := map[string]*harness.Fixture{}
	var started []string
	var firstWaiting string

	stateOf := func(ss *harness.Services, ns string) string {
		if svc := ss.Service(ns, "v1"); svc != nil {
			if in := svc.Instance(ns + "-1"); in != nil {
				return in.State
			}
		}
		return "absent"
	}

	for i, ns := range order {
		t.Run(fmt.Sprintf("start %d %s", i+1, ns), func(t *testing.T) {
			var deps []string
			for _, d := range fleet[ns] {
				deps = append(deps, d+":v1")
			}
			fixtures[ns] = env.Fixture(gw, harness.FixtureOpts{
				Namespace: ns, Instance: ns + "-1", Depends: deps, Owner: "team-" + ns, NoWait: true,
			})
			admin.WaitInstanceAny(ns, "v1", ns+"-1", wait)
			started = append(started, ns)
			want := readySet(started)

			// Every started service settles in the state the gate rules give.
			harness.Eventually(t, wait, "fleet settles", func() error {
				ss, err := admin.Services(ctx)
				if err != nil {
					return err
				}
				for _, s := range started {
					w := "waiting"
					if want[s] {
						w = "ready"
					}
					if got := stateOf(ss, s); got != w {
						return fmt.Errorf("%s: state %s, want %s", s, got, w)
					}
				}
				return nil
			})

			// The gauges say the same: waiting instances and unmet edges.
			for _, s := range started {
				w, r := 1.0, 0.0
				if want[s] {
					w, r = 0, 1
				}
				lab := harness.Labels{"namespace": s, "version": "v1"}
				gw.WaitGauge(t, "service_instances", withLabel(lab, "state", "waiting"), w, wait)
				gw.WaitGauge(t, "service_instances", withLabel(lab, "state", "ready"), r, wait)
				for _, d := range fleet[s] {
					u := 1.0
					if want[d] {
						u = 0
					}
					gw.WaitGauge(t, "dependency_unmet",
						harness.Labels{"consumer_namespace": s, "consumer_version": "v1", "provider": d}, u, wait)
				}
				if !want[s] && firstWaiting == "" {
					firstWaiting = s
				}
			}
			// A waiting service is not routable: its field is not in the schema.
			for _, s := range started {
				if !want[s] {
					if _, err := fixtures[s].Echo(ctx, "", "too early"); err == nil {
						t.Errorf("%s answered while waiting", s)
					}
				}
			}
		})
	}

	t.Run("fleet converges", func(t *testing.T) {
		if firstWaiting == "" {
			t.Fatal("no service ever waited; the shuffle should guarantee two")
		}
		gw.WaitGauge(t, "service_instances", harness.Labels{"state": "waiting"}, 0, wait)
		gw.WaitGauge(t, "service_instances", harness.Labels{"state": "ready"}, float64(len(fleet)), wait)
		gw.WaitGauge(t, "dependency_unmet", harness.Labels{}, 0, wait)
		s := gw.MustScrape(t)
		edges := 0
		for _, ds := range fleet {
			edges += len(ds)
		}
		if got := len(s.Samples("dependency_unmet", nil)); got != edges {
			t.Errorf("dependency_unmet has %d series, want one per edge (%d)\n%s", got, edges, s.Describe("dependency_unmet", nil))
		}

		// Everything answers now, including the ones that waited.
		for ns, f := range fixtures {
			if e, err := f.Echo(ctx, "", "hello"); err != nil || e.Instance != ns+"-1" {
				t.Errorf("%s: echo %+v, %v", ns, e, err)
			}
		}
		g, err := admin.Graph(ctx, "1m")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range g.DesiredEdges {
			if !e.Met {
				t.Errorf("desired edge %s -> %s unmet: %+v", e.ConsumerNamespace, e.Provider, e)
			}
		}
		if len(g.DesiredEdges) != edges {
			t.Errorf("graph has %d desired edges, want %d", len(g.DesiredEdges), edges)
		}
	})

	t.Run("gate transitions", func(t *testing.T) {
		for _, ns := range order {
			lab := func(from, to string) harness.Labels {
				return harness.Labels{"namespace": ns, "version": "v1", "from": from, "to": to}
			}
			var wants []harness.Want
			if isLate[ns] {
				wants = []harness.Want{
					{Labels: lab("none", "waiting"), Delta: 1},
					{Labels: lab("waiting", "ready"), Delta: 1},
					{Labels: lab("none", "ready"), Delta: 0},
				}
			} else {
				wants = []harness.Want{
					{Labels: lab("none", "ready"), Delta: 1},
					{Labels: lab("none", "waiting"), Delta: 0},
					{Labels: lab("waiting", "ready"), Delta: 0},
				}
			}
			wants = append(wants,
				harness.Want{Labels: harness.Labels{"namespace": ns, "to": "degraded"}, Delta: 0},
				harness.Want{Labels: harness.Labels{"namespace": ns, "to": "removed"}, Delta: 0})
			gw.WaitDeltas(t, before, "start_gate_transitions_total", wants, wait)
		}
		after := gw.MustScrape(t)
		if got := harness.Delta(before, after, "start_gate_transitions_total", harness.Labels{"to": "ready"}); got != float64(len(fleet)) {
			t.Errorf("transitions to ready: %v, want %d\n%s", got, len(fleet), after.Describe("start_gate_transitions_total", nil))
		}
		if got := harness.Delta(before, after, "start_gate_transitions_total", harness.Labels{"from": "waiting", "to": "ready"}); got != float64(len(late)) {
			t.Errorf("waiting->ready: %v, want %d (late starters %v)", got, len(late), late)
		}
	})

	t.Run("a registration that closes a cycle is refused", func(t *testing.T) {
		before := gw.MustScrape(t)
		// db would depend on checkout, which depends (through auth) on db.
		cyc := env.Fixture(gw, harness.FixtureOpts{Namespace: "db", Instance: "db-2", Depends: []string{"checkout:v1"}, NoWait: true})
		cyc.Proc.WaitLog("depends close a cycle", wait)
		if code := cyc.Proc.WaitExit(wait); code == 0 {
			t.Errorf("db-2 exit code 0, want a failure")
		}
		path := cyclePath(t, cyc.Proc.Logs(), "db/v1")
		t.Logf("refused: %s", strings.Join(path, " -> "))
		checkCycleIsDeclared(t, path)

		// A shorter cycle: orders also depending on checkout.
		cyc2 := env.Fixture(gw, harness.FixtureOpts{Namespace: "orders", Instance: "orders-2",
			Depends: []string{"auth:v1", "catalog:v1", "checkout:v1"}, NoWait: true})
		cyc2.Proc.WaitLog("depends close a cycle", wait)
		cyc2.Proc.WaitExit(wait)
		path2 := cyclePath(t, cyc2.Proc.Logs(), "orders/v1")
		t.Logf("refused: %s", strings.Join(path2, " -> "))
		if !strings.Contains(strings.Join(path2, " "), "checkout/v1") {
			t.Errorf("cycle %v does not name checkout/v1", path2)
		}

		// Neither registration left anything behind.
		ss, err := admin.Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, ns := range order {
			svc := ss.Service(ns, "v1")
			if svc == nil || len(svc.Instances) != 1 || svc.Instances[0].InstanceID != ns+"-1" || svc.Instances[0].State != "ready" {
				t.Errorf("%s/v1 after the refusals: %+v", ns, svc)
			}
		}
		gw.WaitGauge(t, "service_instances", harness.Labels{"state": "ready"}, float64(len(fleet)), wait)
		gw.WaitGauge(t, "dependency_unmet", harness.Labels{}, 0, wait)
		after := gw.MustScrape(t)
		if got := harness.Delta(before, after, "start_gate_transitions_total", harness.Labels{"to": "removed"}); got != 0 {
			t.Errorf("a refused registration removed %v instances", got)
		}

		// A new leaf on top of the fleet is not a cycle and is accepted.
		reports := env.Fixture(gw, harness.FixtureOpts{Namespace: "reports", Instance: "reports-1", Depends: []string{"checkout:v1"}})
		if e, err := reports.Echo(ctx, "", "ok"); err != nil || e.Instance != "reports-1" {
			t.Errorf("reports echo %+v, %v", e, err)
		}
	})
}

func withLabel(l harness.Labels, kv ...string) harness.Labels {
	out := harness.Labels{}
	for k, v := range l {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

var cycleRE = regexp.MustCompile(`depends close a cycle: ([A-Za-z0-9_/ ->]+?)(?:\s*$|\n)`)

// cyclePath extracts "a/v1 -> b/v1 -> ... -> a/v1" from a process log
// and checks that it starts and ends at start.
func cyclePath(t *testing.T, logs, start string) []string {
	t.Helper()
	m := cycleRE.FindStringSubmatch(logs)
	if m == nil {
		t.Fatalf("no cycle path in the log:\n%s", logs)
	}
	path := strings.Split(m[1], " -> ")
	if len(path) < 3 || path[0] != start || path[len(path)-1] != start {
		t.Fatalf("cycle %v: want a path from %s back to %s", path, start, start)
	}
	return path
}

// checkCycleIsDeclared requires every hop of path to be a declared
// edge of the fleet, or the refused db -> checkout edge.
func checkCycleIsDeclared(t *testing.T, path []string) {
	t.Helper()
	for i := 0; i+1 < len(path); i++ {
		from := strings.TrimSuffix(path[i], "/v1")
		to := strings.TrimSuffix(path[i+1], "/v1")
		ok := from == "db" && to == "checkout"
		for _, d := range fleet[from] {
			if d == to {
				ok = true
			}
		}
		if !ok {
			t.Errorf("cycle hop %s -> %s is not a declared dependency", path[i], path[i+1])
		}
	}
}
