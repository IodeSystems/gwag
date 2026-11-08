//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

// TestS12KafkaEvents: typed event channels over Kafka with the
// Schema Registry, services registered on a 3-node gwag cluster.
//
// The event traffic never touches the gateway: orders publishes to
// Kafka, the listeners read from Kafka. The gateway still knows the
// channels (orders declares them in its registration) and every
// listener (each registers its Subscribes), so the start gate, the
// dependency graph and the sunset rule work as on JetStream (S11).
//
// The Go services are e2e/kafka-fixture, a module of its own run as
// processes, so this package does not import franz-go. TypeScript and
// Python have no control-plane client; the test registers their
// bindings, with the same Subscribes, through controlclient.
//
// Needs docker and E2E_KAFKA=1: it starts apache/kafka and
// confluentinc/cp-schema-registry (the tags examples/events pins).
func TestS12KafkaEvents(t *testing.T) {
	if os.Getenv("E2E_KAFKA") != "1" {
		t.Skip("S12 starts Kafka and a Schema Registry in docker; set E2E_KAFKA=1 to run it")
	}
	if why := harness.DockerAvailable(); why != "" {
		t.Skip("S12 needs docker: " + why)
	}
	env := harness.New(t)
	ctx := harness.Ctx(t, 15*time.Minute)
	stack := harness.StartKafkaStack(t)
	cluster := env.Cluster(3, harness.GatewayOpts{})
	n1, n2, n3 := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]
	fx := harness.Binary(t, "kafka-fixture")

	root, err := harness.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	ordersV1 := filepath.Join(root, "examples/events/proto/orders/v1/orders.proto")
	ordersV2 := filepath.Join(env.Dir(), "orders_v2.proto")
	if err := os.WriteFile(ordersV2, s11OrdersV2(t, s11Read(t, root, "examples/events/proto/orders/v1/orders.proto")), 0o644); err != nil {
		t.Fatal(err)
	}
	kafkaArgs := []string{"--brokers", stack.Brokers, "--sr", stack.SRURL}

	// ---- 1. start gate, graph, sunset -----------------------------------
	// billing: two replicas in group "billing", registered on n1. Each
	// subscribes on Kafka first, then registers; SelfRegister waits for
	// orders, which declares the channels.
	var billing []*harness.Proc
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("billing-%d", i)
		args := append([]string{"billing"}, kafkaArgs...)
		args = append(args, "--gateway", n1.CPAddr(), "--gateway-alt", n2.CPAddr(), "--gateway-alt", n3.CPAddr(),
			"--instance", id, "--group", "billing")
		p := env.Start(id, fx, args...)
		p.WaitLog("SUBSCRIBED", 2*time.Minute)
		billing = append(billing, p)
	}
	if !t.Run("billing waits for the channels' declarer", func(t *testing.T) {
		for i, p := range billing {
			id := fmt.Sprintf("billing-%d", i+1)
			n1.Admin().WaitInstance("billing", "v1", id, "waiting", 30*time.Second)
			if strings.Contains(p.CurrentLogs(), "REGISTERED") {
				t.Fatalf("%s: SelfRegister returned before orders registered", id)
			}
		}
	}) {
		return
	}

	ordersArgs := append([]string{"orders"}, kafkaArgs...)
	ordersArgs = append(ordersArgs, "--gateway", n2.CPAddr(), "--gateway-alt", n3.CPAddr(), "--instance", "orders-1",
		"--proto-v1", ordersV1, "--proto-v2", ordersV2, "--options", filepath.Join(root, "gw/proto/ps/v1/options.proto"),
		"--control", fmt.Sprintf("127.0.0.1:%d", harness.FreePort(t)))
	orders := env.Start("orders-1", fx, ordersArgs...)
	orders.WaitLog("LISTENING", 30*time.Second)
	producer := &s12Producer{base: "http://" + s12Field(t, orders.CurrentLogs(), "LISTENING control=")}

	if !t.Run("orders registers, billing becomes ready", func(t *testing.T) {
		orders.WaitLog("REGISTERED orders-1", 30*time.Second)
		for i, p := range billing {
			id := fmt.Sprintf("billing-%d", i+1)
			p.WaitLog("REGISTERED "+id, 30*time.Second)
			for _, g := range cluster.Nodes {
				g.Admin().WaitInstance("billing", "v1", id, "ready", 30*time.Second)
			}
		}
	}) {
		return
	}

	// TypeScript and Python listeners, each in its own group. Started
	// before the first publish: a new group starts at new messages.
	tsL := s12StartTS(t, env, root, stack)
	pyL := s12StartPy(t, env, root, stack)
	svcAddr := s11GRPCServer(t)
	listeners := map[string]string{} // namespace -> group
	for _, l := range []struct {
		s         *s11Script
		ns, group string
		g         *harness.Gateway
	}{{tsL, "tsnotify", "ts", n2}, {pyL, "pynotify", "py", n3}} {
		if l.s.skip != "" {
			continue
		}
		var subs []controlclient.Subscription
		for _, c := range s11Channels {
			subs = append(subs, controlclient.Subscription{Channel: "orders.v1." + c, Group: l.group})
		}
		reg := s11Register(t, ctx, controlclient.Options{
			GatewayAddr: l.g.CPAddr(),
			ServiceAddr: svcAddr,
			InstanceID:  l.ns + "-1",
			Services: []controlclient.Service{{
				Namespace:   l.ns,
				Version:     "v1",
				ProtoSource: []byte(fmt.Sprintf("syntax = \"proto3\";\npackage %s.v1;\nmessage Ping { string id = 1; }\nservice Notify { rpc Get(Ping) returns (Ping); }\n", l.ns)),
				Subscribes:  subs,
			}},
		})
		s11WaitReg(t, reg, l.ns+"-1", 30*time.Second)
		listeners[l.ns] = l.group
	}

	t.Run("graph has channel edges and orders/v1 is not sunset-able", func(t *testing.T) {
		harness.Eventually(t, 30*time.Second, "n1 graph shows every listener's channel edges", func() error {
			gr, err := n1.Admin().Graph(ctx, "")
			if err != nil {
				return err
			}
			want := map[string]struct {
				group string
				n     int
			}{"billing": {"billing", 2}}
			for ns, g := range listeners {
				want[ns] = struct {
					group string
					n     int
				}{g, 1}
			}
			got := map[string]bool{}
			for _, e := range gr.DesiredEdges {
				w, ok := want[e.ConsumerNamespace]
				if !ok {
					continue
				}
				if e.EdgeKind != "channel" || e.Group != w.group || e.Provider != "orders" ||
					strings.Join(e.Lines, ",") != "v1" || !e.Met || e.InstanceCount != w.n {
					return fmt.Errorf("%s edge %+v", e.ConsumerNamespace, e)
				}
				got[e.ConsumerNamespace+" "+e.Channel] = true
			}
			for ns := range want {
				for _, c := range s11Channels {
					if !got[ns+" orders.v1."+c] {
						return fmt.Errorf("no %s edge for orders.v1.%s (have %v)", ns, c, got)
					}
				}
			}
			v1, v2 := gr.Node("orders/v1"), gr.Node("orders/v2")
			if v1 == nil || v2 == nil {
				return errors.New("orders/v1 or orders/v2 missing from the graph")
			}
			if v1.Sunsetable || !strings.Contains(v1.SunsetReason, "billing/v1 subscribes to orders.v1.") {
				return fmt.Errorf("orders/v1 sunsetable=%v reason=%q", v1.Sunsetable, v1.SunsetReason)
			}
			if !v2.Sunsetable {
				return fmt.Errorf("orders/v2 has no subscriber, want sunset-able: %q", v2.SunsetReason)
			}
			return nil
		})
	})

	// ---- 2. delivery ----------------------------------------------------
	const nTyped = 70
	typed := producer.publish(t, "typed", nTyped, 0)
	t.Run("the billing group gets each event once, both replicas work", func(t *testing.T) {
		st := s12WaitDelivered(t, billing, typed, 90*time.Second)
		t.Logf("typed: %d events, %d deliveries, %d duplicated, per replica %v", nTyped, st.deliveries, st.duplicated, st.perReplica)
		if st.duplicated > 0 {
			t.Logf("typed: duplicated ids %v", st.dupIDs)
		}
		for _, p := range billing {
			if st.perReplica[p.Name] == 0 {
				t.Errorf("%s got no work: %v", p.Name, st.perReplica)
			}
		}
	})
	for _, l := range []*s11Script{tsL, pyL} {
		t.Run(l.lang+" listener gets every event", func(t *testing.T) {
			if l.skip != "" {
				t.Skip(l.skip)
			}
			s12WaitLines(t, l, typed, 90*time.Second)
		})
	}

	// ---- 3. Confluent reads what gwag wrote --------------------------------
	t.Run("Confluent's Java deserializer reads every record", func(t *testing.T) {
		want := map[string]bool{}
		for i := 0; i < nTyped; i++ {
			want[s12ConfluentJSON(i, "typed")] = true
		}
		// The console consumer resolves each record's schema id and
		// message index in the registry: the union channel's two types
		// and the oneof come back as what they are.
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		out, err := stack.Exec(cctx, "timeout", "150", "kafka-protobuf-console-consumer",
			"--bootstrap-server", stack.Name+":9094",
			"--include", "^EV_orders_v1_(OrderShipped|Payments|OrderEvents|Audit)$",
			"--from-beginning", "--max-messages", strconv.Itoa(nTyped),
			"--property", "schema.registry.url=http://localhost:8081")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "{") {
				got[strings.TrimSpace(l)]++
			}
		}
		var missing, extra []string
		for w := range want {
			if got[w] == 0 {
				missing = append(missing, w)
			}
		}
		for g := range got {
			if !want[g] {
				extra = append(extra, g)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("Confluent JSON: %d missing %v; %d unexpected %v", len(missing), s11Head(missing, 5), len(extra), s11Head(extra, 5))
		}
		t.Logf("Confluent read %d records, e.g. %s", len(got), s12Any(got))
	})

	// ---- 4. import: a topic another team writes ----------------------------
	t.Run("import: gwag gen --registry code reads Confluent-serialized corp-orders", func(t *testing.T) {
		s12CorpImport(t, ctx, env, root, stack)
	})

	// ---- 6. retry topic and dead letter -------------------------------------
	t.Run("RetryTopic: attempts on the retry topic, then the DLQ with Gwag-Error", func(t *testing.T) {
		probeArgs := append([]string{"retry-probe"}, kafkaArgs...)
		probe := env.Start("retry-probe", fx, append(probeArgs, "--group", "dlq-probe")...)
		probe.WaitLog("READY", 2*time.Minute)
		producer.publish(t, "dlq", 1, 0) // dlq-0: OrderShipped
		probe.WaitLog("DLQ dlq-0 ", 60*time.Second)
		count := func(prefix string) int {
			n := 0
			for _, l := range strings.Split(probe.CurrentLogs(), "\n") {
				if strings.HasPrefix(l, prefix) {
					n++
				}
			}
			return n
		}
		// Settle: a 4th attempt would follow within the backoff.
		harness.Consistently(t, 2*time.Second, "no delivery after the dead letter", func() error {
			if n := count("ATTEMPT dlq-0 "); n != 3 {
				return fmt.Errorf("%d attempts, want MaxDeliver 3", n)
			}
			return nil
		})
		logs := probe.CurrentLogs()
		for _, want := range []string{
			"ATTEMPT dlq-0 1\n", "ATTEMPT dlq-0 2\n", "ATTEMPT dlq-0 3\n",
			"RETRY dlq-0 attempt=2\n", "RETRY dlq-0 attempt=3\n",
			"DLQ dlq-0 key=events.orders.dlq-0.shipped type=orders.v1.OrderShipped channel=orders.v1.OrderShipped attempt= error=s12: handler always fails body=dlq-0\n",
		} {
			if !strings.Contains(logs, want) {
				t.Errorf("retry-probe output has no %q", strings.TrimSpace(want))
			}
		}
		if n := count("RETRY dlq-0 "); n != 2 {
			t.Errorf("%d retry records, want 2", n)
		}
		if n := count("DLQ "); n != 1 {
			t.Errorf("%d dead letters, want 1", n)
		}
		if t.Failed() {
			t.Logf("retry-probe output:\n%s", logs)
		}
	})

	// ---- 5. gateway node failure ---------------------------------------------
	t.Run("killing billing's gateway node: Kafka delivery goes on, registrations move", func(t *testing.T) {
		oldReg := map[string]string{}
		ss, err := n2.Admin().Services(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if svc := ss.Service("billing", "v1"); svc != nil {
			for _, in := range svc.Instances {
				oldReg[in.InstanceID] = in.RegistrationID
			}
		}
		if len(oldReg) != 2 {
			t.Fatalf("n2 lists billing instances %v, want 2", oldReg)
		}

		producer.start(t, "fo", 60, 50*time.Millisecond)
		harness.Eventually(t, 60*time.Second, "20 failover events acked", func() error {
			if b := producer.acked(t, "fo"); len(b.Acked) < 20 {
				return fmt.Errorf("%d acked", len(b.Acked))
			}
			return nil
		})
		n1.Proc.Kill()
		killedAt := time.Now()
		t.Logf("killed n1 after %d acked publishes", len(producer.acked(t, "fo").Acked))
		res := producer.wait(t, "fo", 60*time.Second)
		if res.Errors > 0 {
			t.Errorf("%d publish errors with a gateway node down (Kafka publish does not use the gateway)", res.Errors)
		}
		st := s12WaitDelivered(t, billing, res.Acked, 90*time.Second)
		t.Logf("failover: %d deliveries, %d duplicated %v, per replica %v", st.deliveries, st.duplicated, st.dupIDs, st.perReplica)
		for _, l := range []*s11Script{tsL, pyL} {
			if l.skip == "" {
				s12WaitLines(t, l, res.Acked, 90*time.Second)
			}
		}

		survivors := []*harness.Gateway{n2, n3}
		harness.Eventually(t, 60*time.Second, "billing re-registered through a survivor, ready on both", func() error {
			for _, g := range survivors {
				ss, err := g.Admin().Services(ctx)
				if err != nil {
					return err
				}
				svc := ss.Service("billing", "v1")
				if svc == nil {
					return fmt.Errorf("%s: billing/v1 missing", g.Name)
				}
				for id, old := range oldReg {
					moved := false
					for _, in := range svc.Instances {
						if in.InstanceID == id && in.RegistrationID != old && in.State == "ready" {
							moved = true
						}
					}
					if !moved {
						return fmt.Errorf("%s: %s has no new ready registration (old %s): %+v", g.Name, id, old, svc.Instances)
					}
				}
			}
			return nil
		})
		t.Logf("billing registrations moved off n1 within %s of the kill", time.Since(killedAt).Round(time.Millisecond))
		for _, g := range survivors {
			g.Admin().WaitInstance("orders", "v1", "orders-1", "ready", 10*time.Second)
		}
		gr, err := n2.Admin().Graph(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if v1 := gr.Node("orders/v1"); v1 == nil || v1.Sunsetable {
			t.Errorf("after the move orders/v1 is %+v, want not sunset-able", v1)
		}
	})
}

// ---- producer control ------------------------------------------------------

// s12Producer drives kafka-fixture orders over its control listener.
type s12Producer struct{ base string }

type s12Batch struct {
	Acked  map[string]string `json:"acked"` // msg id -> listener line
	Done   bool              `json:"done"`
	Errors int               `json:"errors"`
}

func (p *s12Producer) start(t *testing.T, prefix string, n int, interval time.Duration) {
	t.Helper()
	resp, err := http.Post(fmt.Sprintf("%s/publish?prefix=%s&n=%d&interval=%s", p.base, prefix, n, interval), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("publish %s: status %d", prefix, resp.StatusCode)
	}
}

func (p *s12Producer) acked(t *testing.T, prefix string) s12Batch {
	t.Helper()
	resp, err := http.Get(p.base + "/acked?prefix=" + prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b s12Batch
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (p *s12Producer) wait(t *testing.T, prefix string, timeout time.Duration) s12Batch {
	t.Helper()
	var b s12Batch
	harness.Eventually(t, timeout, "batch "+prefix+" published", func() error {
		if b = p.acked(t, prefix); !b.Done {
			return fmt.Errorf("%d acked, %d errors", len(b.Acked), b.Errors)
		}
		return nil
	})
	return b
}

// publish publishes prefix-0..prefix-(n-1) and returns id -> line once
// every one is acked.
func (p *s12Producer) publish(t *testing.T, prefix string, n int, interval time.Duration) map[string]string {
	t.Helper()
	p.start(t, prefix, n, interval)
	b := p.wait(t, prefix, 60*time.Second)
	if b.Errors > 0 {
		t.Logf("batch %s: %d publish errors retried", prefix, b.Errors)
	}
	return b.Acked
}

// ---- delivery accounting ------------------------------------------------------

type s12Delivery struct {
	count   int
	line    string
	replica map[string]int
}

// s12Deliveries parses "DELIVER <id> <n> | <line>" from every proc.
func s12Deliveries(procs []*harness.Proc) map[string]*s12Delivery {
	out := map[string]*s12Delivery{}
	for _, p := range procs {
		for _, l := range strings.Split(p.CurrentLogs(), "\n") {
			rest, ok := strings.CutPrefix(l, "DELIVER ")
			if !ok {
				continue
			}
			head, line, ok := strings.Cut(rest, " | ")
			if !ok {
				continue
			}
			id, _, _ := strings.Cut(head, " ")
			d := out[id]
			if d == nil {
				d = &s12Delivery{replica: map[string]int{}}
				out[id] = d
			}
			d.count++
			d.line = line
			d.replica[p.Name]++
		}
	}
	return out
}

// s12WaitDelivered waits until every id in want reached the group and
// checks each carried its line. It reports duplicates in the stats.
func s12WaitDelivered(t *testing.T, procs []*harness.Proc, want map[string]string, timeout time.Duration) s11Stats {
	t.Helper()
	harness.Eventually(t, timeout, fmt.Sprintf("billing receives all %d events", len(want)), func() error {
		got := s12Deliveries(procs)
		var missing []string
		for id := range want {
			if got[id] == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%d of %d not delivered: %v", len(missing), len(want), s11Head(missing, 10))
		}
		return nil
	})
	got := s12Deliveries(procs)
	st := s11Stats{perReplica: map[string]int{}}
	for id, line := range want {
		d := got[id]
		if d.line != line {
			t.Errorf("%s: delivered as %q, want %q", id, d.line, line)
		}
		st.deliveries += d.count
		if d.count > 1 {
			st.duplicated++
			st.dupIDs = append(st.dupIDs, fmt.Sprintf("%s x%d", id, d.count))
		}
		for r, c := range d.replica {
			st.perReplica[r] += c
		}
	}
	sort.Strings(st.dupIDs)
	return st
}

// s12WaitLines waits until listener l printed every line of want.
func s12WaitLines(t *testing.T, l *s11Script, want map[string]string, timeout time.Duration) {
	t.Helper()
	harness.Eventually(t, timeout, l.lang+" listener prints every event", func() error {
		got := l.lines()
		var missing []string
		for _, w := range want {
			if got[w] == 0 {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("%d of %d missing, e.g. %q", len(missing), len(want), missing[0])
		}
		return nil
	})
	got, dup := l.lines(), 0
	for _, w := range want {
		if got[w] > 1 {
			dup++
		}
	}
	t.Logf("%s: %d events, %d duplicated", l.lang, len(want), dup)
}

// s12ConfluentJSON is event i (kafka-fixture event, s11Event) as
// Confluent's protobuf console consumer prints it: proto3 JSON, fields
// in field-number order, int64 as a string.
func s12ConfluentJSON(i int, prefix string) string {
	oid := fmt.Sprintf("%s-%d", prefix, i)
	switch i % 7 {
	case 0:
		return fmt.Sprintf(`{"orderId":%q,"carrier":"ups"}`, oid)
	case 1:
		return fmt.Sprintf(`{"captured":{"orderId":%q,"amountCents":"%d"}}`, oid, i)
	case 2:
		return fmt.Sprintf(`{"refunded":{"orderId":%q,"amountCents":"%d","reason":%q}}`, oid, i, "r"+oid)
	case 3:
		return fmt.Sprintf(`{"note":%q}`, "n"+oid)
	case 4:
		return fmt.Sprintf(`{"orderId":%q,"customer":"cust%d"}`, oid, i)
	case 5:
		return fmt.Sprintf(`{"orderId":%q,"reason":"why%d"}`, oid, i)
	default:
		return fmt.Sprintf(`{"actor":"actor%d","action":"act"}`, i)
	}
}

// ---- import -------------------------------------------------------------------

// s12CorpImport: Confluent's Python ProtobufSerializer writes two
// corp.orders.v1.Order records to corp-orders (no Gwag-* headers, a
// schema gwag does not own, with a reference). gwag gen --registry
// pins the subject and writes typed Go code; examples/events/go-kafka/
// corp, built from a copy in a temp directory, reads the records typed.
func s12CorpImport(t *testing.T, ctx context.Context, env *harness.Env, root string, stack *harness.KafkaStack) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH (the Confluent Python serializer runs with uv)")
	}
	confluent := filepath.Join(root, "examples/events/go-kafka/confluent")
	if why := s11Setup(ctx, confluent, "uv", "sync", "--locked", "--quiet"); why != "" {
		t.Skip(why)
	}
	s12Run(t, ctx, confluent, stack.Env(), filepath.Join(confluent, ".venv", "bin", "python"), "confluent_check.py", "produce-corp", "corp-orders")

	// A copy of the corp module whose replaces point at this checkout.
	src := filepath.Join(root, "examples/events/go-kafka/corp")
	dir := filepath.Join(env.Dir(), "corp")
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"gen", "imported"} {
		_ = os.RemoveAll(filepath.Join(dir, d))
	}
	s12Run(t, ctx, dir, nil, "go", "mod", "edit",
		"-replace", "github.com/iodesystems/gwag/v2="+root,
		"-replace", "github.com/iodesystems/gwag/v2/events/kafka="+filepath.Join(root, "events/kafka"))
	// The commands of corp/gen.sh, from the repo root: gwag gen runs
	// protoc-gen-go as a go tool of the root module.
	const m = "github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp"
	gwag := harness.Binary(t, "gwag")
	s12Run(t, ctx, root, nil, gwag, "gen", "--registry", stack.SRURL, "--subject", "corp-orders-value",
		"--import-dir", filepath.Join(dir, "imported"), "--lang", "go", "--out", dir, "--opt", "module="+m,
		"--opt", "Mregistry/corp-orders-value.proto="+m+"/gen/corpordersv1;corpordersv1",
		"--opt", "Mcorp/common/v1/money.proto="+m+"/gen/corpcommonv1;corpcommonv1")
	s12Run(t, ctx, root, nil, gwag, "gen", "--lang", "go", "--out", dir, "-I", filepath.Join(dir, "shadow"), "--opt", "module="+m, "shop/v1/order.proto")
	s12Run(t, ctx, dir, nil, "go", "build", "-o", "corp", ".")

	out := s12Run(t, ctx, dir, stack.Env(), filepath.Join(dir, "corp"), "import", "corp-orders")
	for _, want := range []string{
		"import corp.orders.v1.Order o-3001 total 4200 EUR status PLACED skus [sku-1] (key o-3001",
		"import corp.orders.v1.Order o-3002 total 999 EUR status PLACED skus [sku-2 sku-3] (key o-3002",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("corp import output has no %q:\n%s", want, out)
		}
	}
	lock := s12Run(t, ctx, root, nil, gwag, "gen", "--check-lock", "--import-dir", filepath.Join(dir, "imported"))
	if !strings.Contains(lock, "matches "+stack.SRURL+" (1 subjects)") {
		t.Errorf("gwag gen --check-lock: %s", lock)
	}
}

// s12Run runs a command to completion and returns its combined output;
// the test fails if it exits non-zero.
func s12Run(t *testing.T, ctx context.Context, dir string, envv []string, name string, args ...string) string {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	if envv != nil {
		cmd.Env = append(os.Environ(), envv...)
	}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, b)
	}
	return string(b)
}

// ---- TypeScript and Python listeners ------------------------------------------

// s12StartTS starts examples/events/ts-kafka/src/listen.ts in group
// "ts". It needs node >= 24 and pnpm.
func s12StartTS(t *testing.T, env *harness.Env, root string, stack *harness.KafkaStack) *s11Script {
	t.Helper()
	s := &s11Script{lang: "TypeScript"}
	node, err := exec.LookPath("node")
	if err != nil {
		s.skip = "node not on PATH"
		return s
	}
	if _, err := exec.LookPath("pnpm"); err != nil {
		s.skip = "pnpm not on PATH"
		return s
	}
	v, err := exec.Command(node, "--version").Output()
	if err != nil {
		s.skip = fmt.Sprintf("node --version: %v", err)
		return s
	}
	if major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(string(v)), "v"), ".", 2)[0]); major < 24 {
		s.skip = fmt.Sprintf("node %s: the example needs node >= 24", strings.TrimSpace(string(v)))
		return s
	}
	ctx := harness.Ctx(t, 10*time.Minute)
	for _, dir := range []string{"ts/events", "ts/events-kafka", "ts/events-schemaregistry", "examples/events/ts", "examples/events/ts-kafka"} {
		if why := s11Setup(ctx, filepath.Join(root, dir), "pnpm", "install", "--frozen-lockfile"); why != "" {
			s.skip = why
			return s
		}
	}
	s.proc = env.StartWith("ts-listener", harness.ProcOpts{
		Dir: filepath.Join(root, "examples/events/ts-kafka"),
		Env: stack.Env(),
	}, node, "src/listen.ts", "ts")
	s.proc.WaitLog("ready", 2*time.Minute)
	return s
}

// s12StartPy starts examples/events/py-kafka/listen.py in group "py".
// It needs uv.
func s12StartPy(t *testing.T, env *harness.Env, root string, stack *harness.KafkaStack) *s11Script {
	t.Helper()
	s := &s11Script{lang: "Python"}
	if _, err := exec.LookPath("uv"); err != nil {
		s.skip = "uv not on PATH"
		return s
	}
	dir := filepath.Join(root, "examples/events/py-kafka")
	if why := s11Setup(harness.Ctx(t, 10*time.Minute), dir, "uv", "sync", "--locked", "--quiet"); why != "" {
		s.skip = why
		return s
	}
	s.proc = env.StartWith("py-listener", harness.ProcOpts{
		Dir:   dir,
		Env:   append(stack.Env(), "PYTHONUNBUFFERED=1"),
		Stdin: true,
	}, filepath.Join(dir, ".venv", "bin", "python"), "listen.py", "--group", "py")
	s.proc.WaitLog("READY", 2*time.Minute)
	return s
}

// ---- small helpers -------------------------------------------------------------

// s12Field returns the rest of the first line of logs after prefix.
func s12Field(t *testing.T, logs, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(logs, "\n") {
		if _, v, ok := strings.Cut(l, prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %q in output", prefix)
	return ""
}

func s12Any(m map[string]int) string {
	for k := range m {
		return k
	}
	return ""
}
