//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

const s8Secret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// TestS8SubscriptionFailover: a client holds a ps_sub subscription on
// node A while a publisher on node B sends numbered events. A is
// killed; the client reconnects to node C, subscribes again with the
// same token, and keeps receiving. Events published while the client
// had no subscription are lost (pub/sub has no replay); the story
// measures how many, and requires that the loss is one contiguous
// window around the failover and that nothing is duplicated or
// reordered.
func TestS8SubscriptionFailover(t *testing.T) {
	env := harness.New(t)
	cluster := env.Cluster(3, harness.GatewayOpts{Args: []string{"--subscribe-secret", s8Secret}})
	nodeA, nodeB, nodeC := cluster.Nodes[0], cluster.Nodes[1], cluster.Nodes[2]
	ctx := harness.Ctx(t, 5*time.Minute)

	preScrapes := map[string]*harness.Scrape{}
	for _, g := range cluster.Nodes {
		preScrapes[g.Name] = g.MustScrape(t)
	}

	const channel = "events.s8.orders"
	tok, err := nodeB.Sign(ctx, channel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	subQuery := fmt.Sprintf(`subscription { ps_sub(channel: %q, %s) { channel payload ts } }`, channel, tok.Args())

	// event decodes one next payload to its sequence number; the
	// warm-up probes carry non-numeric payloads and return -1.
	event := func(data json.RawMessage) (int, error) {
		var d struct {
			PsSub struct{ Channel, Payload string } `json:"ps_sub"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return 0, err
		}
		if d.PsSub.Channel != channel {
			return 0, fmt.Errorf("event on channel %q", d.PsSub.Channel)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(d.PsSub.Payload, "seq-"))
		if err != nil || !strings.HasPrefix(d.PsSub.Payload, "seq-") {
			return -1, nil
		}
		return n, nil
	}

	// ---- 1. subscribe on A, publish through B --------------------------
	subA, err := nodeA.Subscribe(t, subQuery)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("subscription is registered on A", func(t *testing.T) {
		harness.Eventually(t, 10*time.Second, "A lists one consumer on the channel", func() error {
			return wantConsumers(ctx, nodeA, channel, 1)
		})
		harness.Eventually(t, 10*time.Second, "A counts one stream", func() error {
			return wantStreams(ctx, nodeA, 1)
		})
		for _, n := range []*harness.Gateway{nodeB, nodeC} {
			if err := wantConsumers(ctx, n, channel, 0); err != nil {
				t.Error(err)
			}
			if err := wantStreams(ctx, n, 0); err != nil {
				t.Error(err)
			}
		}
	})

	// The NATS interest of A's subscription reaches B asynchronously.
	// Probe until one event comes through; probes are not counted.
	var pubs atomic.Int64 // every ps.pub call made through B, ok or not
	pubOnB := func(payload string) error {
		pubs.Add(1)
		return nodeB.Publish(ctx, channel, payload, tok)
	}
	harness.Eventually(t, 20*time.Second, "an event published on B reaches A", func() error {
		if err := pubOnB("warmup"); err != nil {
			return err
		}
		select {
		case <-subA.Events:
			return nil
		case <-time.After(300 * time.Millisecond):
			return fmt.Errorf("no event yet")
		}
	})
	// Late probe deliveries must not be mistaken for numbered events.
	drainWarmup := func(s *harness.Subscription) {
		for {
			select {
			case <-s.Events:
			case <-time.After(200 * time.Millisecond):
				return
			}
		}
	}
	drainWarmup(subA)

	// ---- 2. steady state: numbered events arrive once, in order -------
	type delivery struct {
		seq  int
		node string
	}
	var (
		mu        sync.Mutex
		got       []delivery
		published []int // seqs whose ps.pub returned ok
		pubFailed []string
	)
	record := func(s *harness.Subscription) {
		for {
			select {
			case data := <-s.Events:
				n, err := event(data)
				if err != nil {
					mu.Lock()
					pubFailed = append(pubFailed, "decode: "+err.Error())
					mu.Unlock()
					continue
				}
				if n < 0 {
					continue // warm-up leftover
				}
				mu.Lock()
				got = append(got, delivery{n, s.Node.Name})
				mu.Unlock()
			case <-s.Done:
				// Take what the socket delivered before it ended.
				for {
					select {
					case data := <-s.Events:
						if n, err := event(data); err == nil && n >= 0 {
							mu.Lock()
							got = append(got, delivery{n, s.Node.Name})
							mu.Unlock()
						}
					default:
						return
					}
				}
			}
		}
	}
	recordedA := make(chan struct{})
	go func() { record(subA); close(recordedA) }()

	pubCtx, stopPub := context.WithCancel(context.Background())
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		for seq := 1; pubCtx.Err() == nil; seq++ {
			if err := pubOnB("seq-" + strconv.Itoa(seq)); err != nil {
				mu.Lock()
				pubFailed = append(pubFailed, fmt.Sprintf("seq %d: %v", seq, err))
				mu.Unlock()
			} else {
				mu.Lock()
				published = append(published, seq)
				mu.Unlock()
			}
			select {
			case <-pubCtx.Done():
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { stopPub(); <-pubDone })

	count := func(node string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, d := range got {
			if d.node == node {
				n++
			}
		}
		return n
	}
	harness.Eventually(t, 30*time.Second, "A receives 100 events", func() error {
		if n := count(nodeA.Name); n < 100 {
			return fmt.Errorf("%d events", n)
		}
		return nil
	})
	t.Run("steady state is exactly-once and ordered", func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()
		for i, d := range got {
			if d.seq != i+1 {
				t.Fatalf("delivery %d on A has seq %d, want %d (no loss, duplicate or reorder before the failover)", i, d.seq, i+1)
			}
		}
	})

	// ---- 3. gauges with the subscription open --------------------------
	t.Run("gauges while subscribed", func(t *testing.T) {
		s := nodeA.MustScrape(t)
		if err := wantStreams(ctx, nodeA, 1); err != nil {
			t.Error(err)
		}
		if v := s.Counter("pubsub_bus_degraded", nil); v != 0 {
			t.Errorf("A pubsub_bus_degraded = %v, want 0", v)
		}
		body, status, err := nodeA.Health(ctx)
		if err != nil || status != http.StatusOK || body["pubsub"] != "nats" {
			t.Errorf("A health = %d %v %v, want 200 with pubsub=nats", status, body, err)
		}
	})

	// ---- 4. kill A; the client reconnects to C -------------------------
	nodeA.Proc.Kill()
	killedAt := time.Now()
	select {
	case <-subA.Done:
	case <-time.After(30 * time.Second):
		t.Fatalf("A's socket did not end 30s after SIGKILL")
	}
	<-recordedA
	detected := time.Since(killedAt)
	mu.Lock()
	lastOnA := got[len(got)-1].seq
	mu.Unlock()

	var subC *harness.Subscription
	harness.Eventually(t, 30*time.Second, "client resubscribes on C", func() error {
		s, err := nodeC.Subscribe(t, subQuery)
		if err != nil {
			return err
		}
		subC = s
		return nil
	})
	resubscribed := time.Since(killedAt)
	recordedC := make(chan struct{})
	go func() { record(subC); close(recordedC) }()
	t.Logf("failover: client saw A's socket end %s after SIGKILL, subscription on C was sent %s after SIGKILL", detected.Round(time.Millisecond), resubscribed.Round(time.Millisecond))

	t.Run("subscription is registered on C", func(t *testing.T) {
		harness.Eventually(t, 10*time.Second, "C lists one consumer on the channel", func() error {
			return wantConsumers(ctx, nodeC, channel, 1)
		})
		harness.Eventually(t, 10*time.Second, "C counts one stream", func() error {
			return wantStreams(ctx, nodeC, 1)
		})
		if err := wantStreams(ctx, nodeB, 0); err != nil {
			t.Error(err)
		}
	})
	harness.Eventually(t, 60*time.Second, "C receives 100 events", func() error {
		if n := count(nodeC.Name); n < 100 {
			return fmt.Errorf("%d events", n)
		}
		return nil
	})

	// ---- 5. stop publishing, wait for the tail, measure the gap -------
	stopPub()
	<-pubDone
	mu.Lock()
	lastPublished := published[len(published)-1]
	mu.Unlock()
	harness.Eventually(t, 15*time.Second, "C receives the last published event", func() error {
		mu.Lock()
		defer mu.Unlock()
		if last := got[len(got)-1]; last.seq != lastPublished {
			return fmt.Errorf("last delivered seq %d, last published %d", last.seq, lastPublished)
		}
		return nil
	})

	t.Run("loss is one window around the failover", func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()
		if len(pubFailed) != 0 {
			t.Errorf("publisher/decoder errors: %v", pubFailed)
		}
		var onC []int
		for _, d := range got {
			if d.node == nodeC.Name {
				onC = append(onC, d.seq)
			}
		}
		firstOnC := onC[0]
		if firstOnC <= lastOnA {
			t.Errorf("first event on C is seq %d, not after the last on A (%d)", firstOnC, lastOnA)
		}
		for i := 1; i < len(onC); i++ {
			if onC[i] != onC[i-1]+1 {
				t.Fatalf("C delivered seq %d after %d: loss, duplicate or reorder after the resubscribe", onC[i], onC[i-1])
			}
		}
		lost := firstOnC - lastOnA - 1
		t.Logf("lost %d events during the failover (seq %d..%d; last on A %d, first on C %d; %d published in all)",
			lost, lastOnA+1, firstOnC-1, lastOnA, firstOnC, lastPublished)
		// A client cannot resubscribe before it saw the socket end.
		if resubscribed > 30*time.Second {
			t.Errorf("resubscribe took %s", resubscribed)
		}
	})

	// ---- 6. metrics ---------------------------------------------------
	t.Run("metrics", func(t *testing.T) {
		// Every ps.pub is a graphql request on B, exactly.
		nodeB.WaitDeltas(t, preScrapes[nodeB.Name], "request_duration_seconds_count", []harness.Want{
			{Labels: harness.Labels{"ingress": "graphql"}, Delta: float64(pubs.Load())},
		}, 10*time.Second)
		for _, g := range []*harness.Gateway{nodeB, nodeC} {
			s := g.MustScrape(t)
			if v := s.Counter("pubsub_bus_degraded", nil); v != 0 {
				t.Errorf("%s pubsub_bus_degraded = %v after the failover, want 0", g.Name, v)
			}
			if d := harness.Delta(preScrapes[g.Name], s, "requests_rejected_total", nil); d != 0 {
				t.Errorf("%s requests_rejected_total moved by %v\n%s", g.Name, d, s.Describe("requests_rejected_total", nil))
			}
		}
		// B minted the token through the in-process path, once.
		nodeB.WaitDeltas(t, preScrapes[nodeB.Name], "sign_auth_total", []harness.Want{
			{Labels: harness.Labels{"code": "in_process"}, Delta: 1},
		}, 10*time.Second)
	})

	// ---- 7. the consumer count comes down when the client leaves ------
	t.Run("consumer count falls to zero when the client closes", func(t *testing.T) {
		subC.Close()
		<-recordedC
		harness.Eventually(t, 10*time.Second, "C lists no consumer", func() error {
			return wantConsumers(ctx, nodeC, channel, 0)
		})
		harness.Eventually(t, 10*time.Second, "C counts no stream", func() error {
			return wantStreams(ctx, nodeC, 0)
		})
	})
}

// wantStreams checks the three stream gauges on g: streams_inflight_total,
// pool_streams_inflight{ps,v1} and the health body's active_streams.
func wantStreams(ctx context.Context, g *harness.Gateway, want int) error {
	s, err := g.Scrape(ctx)
	if err != nil {
		return err
	}
	total := s.Counter("streams_inflight_total", nil)
	pool := s.Counter("pool_streams_inflight", harness.Labels{"namespace": "ps", "version": "v1"})
	body, status, err := g.Health(ctx)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("%s health %d %v", g.Name, status, err)
	}
	active, _ := body["active_streams"].(float64)
	if int(total) != want || int(pool) != want || int(active) != want {
		return fmt.Errorf("%s streams: total=%v pool{ps,v1}=%v health=%v, want %d", g.Name, total, pool, active, want)
	}
	return nil
}

// wantConsumers checks the node's /api/admin/channels row for subject.
func wantConsumers(ctx context.Context, g *harness.Gateway, subject string, want int) error {
	var out struct {
		Channels []struct {
			Subject   string `json:"subject"`
			Consumers int    `json:"consumers"`
		} `json:"channels"`
	}
	if err := g.Admin().Do(ctx, http.MethodGet, "/admin/channels", nil, &out); err != nil {
		return err
	}
	got := 0
	for _, c := range out.Channels {
		if c.Subject == subject {
			got = c.Consumers
		}
	}
	if got != want {
		return fmt.Errorf("%s: %d consumers on %s, want %d (%+v)", g.Name, got, subject, want, out.Channels)
	}
	return nil
}
