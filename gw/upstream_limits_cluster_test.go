package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gqlAs(t *testing.T, h http.Handler, caller, query string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"query": query})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if caller != "" {
		req.Header.Set(PublicCallerIDHeader, caller)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Body.String()
}

// vendorFake is a rate-limited upstream: it counts requests per
// wall-clock second and answers 429 over limit. over records any
// second that went past it.
type vendorFake struct {
	limit int
	mu    sync.Mutex
	sec   int64
	n     int
	max   int
	total int
	over  []string
}

func (v *vendorFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	v.mu.Lock()
	if s := now.Unix(); s != v.sec {
		v.sec, v.n = s, 0
	}
	v.n++
	v.total++
	if v.n > v.max {
		v.max = v.n
	}
	over := v.n > v.limit
	if over {
		v.over = append(v.over, now.Format(time.RFC3339Nano))
	}
	v.mu.Unlock()
	if over {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"abc","name":"thing"}`))
}

func waitLimits(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUpstreamLimits_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	opts := []Option{
		WithBackpressure(BackpressureOptions{MaxWaitTime: 2 * time.Second}),
		WithCallerIDPublic(),
		WithUsage(UsageOptions{FlushInterval: 200 * time.Millisecond}),
		WithAdminToken([]byte("tok")),
	}
	a := newSharedClusterNode(t, testCluster.a, opts...)
	b := newSharedClusterNode(t, testCluster.b, opts...)
	waitForPeers(t, a.gw, b.gw)
	nodes := []*clusterNode{a, b}

	vendor := &vendorFake{limit: 10}
	vendorSrv := httptest.NewServer(vendor)
	t.Cleanup(vendorSrv.Close)
	var throttleHits atomic.Int32
	throttle := throttlingBackend(t, &throttleHits, 1, "3")
	var quotaHits atomic.Int32
	quotaBE := countingBackend(t, &quotaHits, nil)
	for _, n := range nodes {
		// Registered on each node in code: the limits apply where
		// the service is registered.
		if err := n.gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(vendorSrv.URL), As("vendor"), RateLimit(6, 2)); err != nil {
			t.Fatal(err)
		}
		if err := n.gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(throttle.URL), As("throttle"), RateLimit(100, 100)); err != nil {
			t.Fatal(err)
		}
		if err := n.gw.AddOpenAPIBytes([]byte(minimalOpenAPISpec), To(quotaBE.URL), As("quota")); err != nil {
			t.Fatal(err)
		}
	}
	waitLimits(t, 5*time.Second, "split over 2 peers on both nodes", func() bool {
		return a.gw.limits.peers.Load() == 2 && b.gw.limits.peers.Load() == 2
	})

	t.Run("two nodes stay under the vendor limit", func(t *testing.T) {
		const q = `{ vendor { getThing(id:"1") { id } } }`
		var ok, refused atomic.Int32
		stop := time.Now().Add(3500 * time.Millisecond)
		var wg sync.WaitGroup
		for _, n := range nodes {
			h := n.gw.Handler()
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for time.Now().Before(stop) {
						body := gqlAs(t, h, "fleet", q)
						switch {
						case strings.Contains(body, `"id":"abc"`):
							ok.Add(1)
						case strings.Contains(body, "rate limit"):
							refused.Add(1)
						default:
							t.Errorf("unexpected answer: %s", body)
							return
						}
					}
				}()
			}
		}
		wg.Wait()
		vendor.mu.Lock()
		defer vendor.mu.Unlock()
		t.Logf("vendor: %d requests, busiest second %d (limit %d); gateway ok %d refused %d", vendor.total, vendor.max, vendor.limit, ok.Load(), refused.Load())
		if len(vendor.over) > 0 {
			t.Fatalf("vendor limit exceeded at %v", vendor.over)
		}
		// 6/s cluster-wide for 3.5s plus the two bursts: about 23.
		if got := ok.Load(); got < 15 {
			t.Errorf("only %d calls got through; the split should allow about 23", got)
		}
	})

	t.Run("upstream 429 pauses the service on the other node", func(t *testing.T) {
		const q = `{ throttle { getThing(id:"1") { id } } }`
		body := gqlAs(t, a.gw.Handler(), "", q)
		if !strings.Contains(body, "429") {
			t.Fatalf("first call: %s", body)
		}
		waitLimits(t, 3*time.Second, "pause on node B", func() bool { return b.gw.limits.pausedFor("throttle", "v1") > 0 })
		body = gqlAs(t, b.gw.Handler(), "", q)
		if !strings.Contains(body, "paused") || !strings.Contains(body, `"retryAfterSeconds":`) {
			t.Fatalf("node B during the pause: %s", body)
		}
		if got := throttleHits.Load(); got != 1 {
			t.Fatalf("upstream got %d calls, want 1", got)
		}
	})

	t.Run("long-window quota counts across nodes", func(t *testing.T) {
		const q = `{ quota { getThing(id:"1") { id } } }`
		rec := limitRecord{Kind: limitKindQuota, Default: true, Namespace: "quota", Limit: 4, PerMs: time.Hour.Milliseconds()}
		if err := a.gw.limits.putRecord(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			gt := n.gw.limits.gate("quota", "v1", "getThing")
			waitLimits(t, 5*time.Second, "quota on every node", func() bool { return len(gt.cfg.Load().quotas) == 1 })
		}
		for i := 0; i < 3; i++ {
			if body := gqlAs(t, a.gw.Handler(), "alice", q); !strings.Contains(body, `"id":"abc"`) {
				t.Fatalf("A call %d: %s", i, body)
			}
		}
		time.Sleep(700 * time.Millisecond) // A flushes 3
		if body := gqlAs(t, b.gw.Handler(), "alice", q); !strings.Contains(body, `"id":"abc"`) {
			t.Fatalf("B call 1 (4th of 4): %s", body)
		}
		time.Sleep(700 * time.Millisecond) // B flushes 1 and reads 4; A reads 4
		for i, n := range nodes {
			if body := gqlAs(t, n.gw.Handler(), "alice", q); !strings.Contains(body, "quota exceeded for caller=alice") {
				t.Fatalf("node %d after 4 of 4: %s", i, body)
			}
		}
		if body := gqlAs(t, b.gw.Handler(), "bob", q); !strings.Contains(body, `"id":"abc"`) {
			t.Fatalf("bob: %s", body)
		}

		// The usage report on either node is the cluster total.
		time.Sleep(500 * time.Millisecond)
		rr := httptest.NewRecorder()
		newAdminRouter(t, a.gw).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/usage?service=quota&caller=alice", nil))
		var rep struct{ Rows []usageRow }
		if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil || len(rep.Rows) != 1 {
			t.Fatalf("usage: %d %s", rr.Code, rr.Body)
		}
		if got := rep.Rows[0].usageCounts; got != (usageCounts{Calls: 6, Units: 4, Quota: 2}) {
			t.Errorf("alice usage %+v, want 6 calls, 4 units, 2 quota refusals", got)
		}
	})
}
