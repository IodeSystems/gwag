package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Stored notifications (notification_store.go): hook-failure items.

func hookFailedItems(t *testing.T, g *Gateway) []notificationTestItem {
	t.Helper()
	var out []notificationTestItem
	for _, it := range notificationItems(t, g) {
		if it.Kind == notificationHookFailed {
			out = append(out, it)
		}
	}
	return out
}

func TestNotifications_DismissDeltaFailure(t *testing.T) {
	sink := newHookSink(t, func(int) int { return http.StatusInternalServerError })
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))

	putExternal(t, g, "acme", acmeV1)
	waitUntil(t, 5*time.Second, "failure item", func() bool { return len(hookFailedItems(t, g)) == 1 })
	it := hookFailedItems(t, g)[0]
	if it.ID != "hook-failed-1-acme" || !strings.Contains(it.Detail, "dismiss") {
		t.Fatalf("item=%+v", it)
	}
	// A later success does not clear a delta-mode failure.
	sink.mu.Lock()
	sink.statuses = nil
	sink.mu.Unlock()
	putExternal(t, g, "beta", acmeV1)
	sink.waitN(t, 4)
	time.Sleep(100 * time.Millisecond)
	if n := len(hookFailedItems(t, g)); n != 1 {
		t.Fatalf("delta failure cleared by a later success: %d items", n)
	}

	code, body := adminDoBearer(t, g, g.DevTokenHex(), http.MethodPost, "/admin/notifications/"+it.ID+"/dismiss", "")
	if code != http.StatusOK || !strings.Contains(body, `"dismissed":true`) {
		t.Fatalf("dismiss: %d %s", code, body)
	}
	if n := len(hookFailedItems(t, g)); n != 0 {
		t.Fatalf("items after dismiss: %d", n)
	}
	if code, _ := adminDoBearer(t, g, g.DevTokenHex(), http.MethodPost, "/admin/notifications/"+it.ID+"/dismiss", ""); code != http.StatusNotFound {
		t.Errorf("dismiss twice: %d, want 404", code)
	}
}

func TestNotifications_DismissRoleAndKind(t *testing.T) {
	sink := newHookSink(t, func(int) int { return http.StatusInternalServerError })
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))
	putExternal(t, g, "acme", `{"kind":"customer","depends":[{"service":"greeter","lines":["v1"]}],"reviewAt":"2020-01-01T00:00:00Z"}`)
	waitUntil(t, 5*time.Second, "failure item", func() bool { return len(hookFailedItems(t, g)) == 1 })

	// The declaration is review-due: that item is computed, not dismissable.
	var due *notificationTestItem
	for _, it := range notificationItems(t, g) {
		if it.Kind == notificationReviewDue {
			it := it
			due = &it
		}
	}
	if due == nil {
		t.Fatal("no review-due item")
	}
	if due.Dismissable {
		t.Errorf("review-due item is dismissable")
	}
	if code, _ := adminDoBearer(t, g, g.DevTokenHex(), http.MethodPost, "/admin/notifications/review-due-acme/dismiss", ""); code != http.StatusNotFound {
		t.Errorf("dismiss review-due: %d, want 404", code)
	}

	h := newAnonAdminRouter(t, g)
	id := hookFailedItems(t, g)[0].ID
	if !hookFailedItems(t, g)[0].Dismissable {
		t.Errorf("hook failure not dismissable")
	}
	if code, _ := rolesDo(t, h, http.MethodPost, "/admin/notifications/"+id+"/dismiss", "", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous dismiss: %d, want 401", code)
	}
	if n := len(hookFailedItems(t, g)); n != 1 {
		t.Errorf("anonymous dismiss removed the item")
	}
	// Admin may dismiss too.
	if code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/notifications/"+id+"/dismiss", ""); code != http.StatusOK {
		t.Errorf("admin dismiss: %d %s", code, body)
	}
}

func TestNotifications_FullModeClearsOnSuccess(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	sink := newHookSink(t, func(int) int {
		if failing.Load() {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsFull, "")))

	putExternal(t, g, "acme", acmeV1)
	putExternal(t, g, "beta", acmeV1)
	waitUntil(t, 5*time.Second, "two failure items", func() bool { return len(hookFailedItems(t, g)) == 2 })
	failing.Store(false)
	putExternal(t, g, "gamma", acmeV1)
	waitUntil(t, 5*time.Second, "failures cleared", func() bool { return len(hookFailedItems(t, g)) == 0 })
}

func TestNotifications_BoundedCountAndSize(t *testing.T) {
	g := newExternalGateway(t)
	ctx := context.Background()
	for i := 1; i <= hookFailureItems+5; i++ {
		err := g.putNotification(ctx, storedNotification{
			notificationItem: notificationItem{
				Kind: notificationHookFailed, ID: "hook-failed-" + strconv.Itoa(i) + "-x", Title: "t",
				Detail: strings.Repeat("é", 5000), Since: "2026-11-07T12:00:00Z", Dismissable: true,
			},
			Seq: uint64(i), Mode: "delta",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	items := g.notifs.all()
	if len(items) != hookFailureItems {
		t.Fatalf("items=%d, want %d", len(items), hookFailureItems)
	}
	if items[0].Seq != 6 || items[len(items)-1].Seq != uint64(hookFailureItems+5) {
		t.Errorf("oldest not evicted: first seq %d last %d", items[0].Seq, items[len(items)-1].Seq)
	}
	for _, n := range items {
		if len(n.Detail) > notificationDetailMax {
			t.Fatalf("detail is %d bytes", len(n.Detail))
		}
		if !strings.HasSuffix(n.Detail, "...") {
			t.Errorf("truncated detail has no marker")
		}
	}
}

// Cluster: three gateways, each with a full-mode hook. A failure on
// one node is listed on every node; a dismiss from any node clears it
// everywhere; a successful delivery from any node clears the rest.
func TestNotifications_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	var failing atomic.Bool
	failing.Store(true)
	sink := newHookSink(t, func(int) int {
		if failing.Load() {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	var nodes []*Gateway
	add := func(cl *Cluster) {
		g := New(WithCluster(cl), WithClusterReplicas(ClusterReplicasAuto), WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")),
			WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsFull, strongTestSecret)))
		t.Cleanup(g.Close)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if _, err := g.startClusterTracking(ctx); err != nil {
			t.Fatalf("node %d: %v", len(nodes), err)
		}
		nodes = append(nodes, g)
	}
	add(testCluster.a)
	add(testCluster.b)
	waitForPeers(t, nodes[0], nodes[1])
	waitUntil(t, 15*time.Second, "buckets at R=2", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, b := range []string{externalDependentsBucketName, notificationsBucketName} {
			s, err := testCluster.a.JS.Stream(ctx, "KV_"+b)
			if err != nil {
				return false
			}
			info, err := s.Info(ctx)
			if err != nil || info.Config.Replicas != 2 {
				return false
			}
		}
		return true
	})
	add(testCluster.a)

	everywhere := func(what string, n int) {
		t.Helper()
		for i, g := range nodes {
			waitUntil(t, 10*time.Second, "node "+strconv.Itoa(i)+" "+what, func() bool { return len(g.notifs.all()) == n })
		}
	}

	// A failure on node 0 shows on every node.
	putExternal(t, nodes[0], "acme", acmeV1)
	everywhere("lists the failure", 1)
	for i, g := range nodes {
		items := hookFailedItems(t, g)
		if len(items) != 1 || !strings.Contains(items[0].Detail, "put acme") {
			t.Fatalf("node %d items=%+v", i, items)
		}
	}
	id := hookFailedItems(t, nodes[0])[0].ID

	// Dismiss from node 2 clears it on every node.
	if code, body := adminDoBearer(t, nodes[2], nodes[2].DevTokenHex(), http.MethodPost, "/admin/notifications/"+id+"/dismiss", ""); code != http.StatusOK {
		t.Fatalf("dismiss on node 2: %d %s", code, body)
	}
	everywhere("is empty after dismiss", 0)

	// A full-mode failure on node 1, then a success from node 2:
	// cleared everywhere, with no dismiss.
	waitUntil(t, 10*time.Second, "beta can be put on node 1", func() bool { _, ok := nodes[1].externals.get("acme"); return ok })
	putExternal(t, nodes[1], "beta", acmeV1)
	everywhere("lists the second failure", 1)
	failing.Store(false)
	waitUntil(t, 10*time.Second, "beta on node 2", func() bool { _, ok := nodes[2].externals.get("beta"); return ok })
	putExternal(t, nodes[2], "gamma", acmeV1)
	everywhere("is empty after a successful delivery", 0)
}
