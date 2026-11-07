//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

// TestS9ExternalDependents: an external API customer, Acme Corp,
// pins library [v1]. gwag cannot see Acme (it registers nothing and
// calls through its own edge), so the books team declares it through
// the admin API. The story:
//
//  1. library v1 is up; Acme is declared, pinning library [v1]
//  2. v2 ships on a separate instance: v1 is not sunset-able, and
//     draining the last v1 instance is blocked with a reason naming
//     Acme
//  3. Acme accepts [v1, v2], then [v2]: v1 becomes sunset-able in the
//     1m graph and the held drain completes; the v1 instance exits
//  4. a pin with no ready line raises dependency_unmet
//     {consumer_kind="external"}
//  5. Acme's review date passes: it still holds library [v2] (drain
//     blocked), shows as review-due in the list, the notifications
//     and declarations_review_due, until the review op sets a new date
//  6. the declaration is deleted: no edge, no series
//
// The start gate is not involved: nothing registers for Acme.
func TestS9ExternalDependents(t *testing.T) {
	env := harness.New(t)
	gw := env.Gateway(harness.GatewayOpts{Environment: "e2e"})
	admin := gw.Admin()
	ctx := harness.Ctx(t, 4*time.Minute)

	const (
		drains = "service_drain_instances"
		unmet  = "dependency_unmet"
		sunset = "line_sunsetable"
		wait   = 20 * time.Second
	)
	lib := func(ver string) harness.Labels { return harness.Labels{"namespace": "library", "version": ver} }
	acmeUnmet := harness.Labels{"consumer_kind": "external", "consumer_namespace": "acme", "consumer_version": "", "provider": "library"}
	acme := func(lines ...string) harness.ExternalDependent {
		x := harness.ExternalDependent{
			ID:      "acme",
			Name:    "Acme Corp",
			Kind:    "customer",
			Depends: []harness.ExternalPin{{Service: "library", Lines: lines}},
			Note:    "contract 2026-14: library v1 until the v2 migration",
			// Real now: the e2e gateway runs on the wall clock.
			ReviewAt: time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}
		x.Owner = &struct {
			Team    string `json:"team,omitempty"`
			Contact string `json:"contact,omitempty"`
		}{Team: "books", Contact: "acme-api@example.com"}
		return x
	}
	put := func(t *testing.T, x harness.ExternalDependent) {
		t.Helper()
		if _, err := admin.PutExternalDependent(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	graph := func(t *testing.T) *harness.Graph {
		t.Helper()
		g, err := admin.Graph(ctx, "1m")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	externalEdge := func(g *harness.Graph) *harness.DesiredEdge {
		for i := range g.DesiredEdges {
			if e := &g.DesiredEdges[i]; e.ConsumerKind == "external" && e.ConsumerNamespace == "acme" {
				return e
			}
		}
		return nil
	}

	libA := env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Instance: "library-a", Versions: []string{"v1"}, Owner: "books", DrainOnSIGTERM: true})

	t.Run("1 Acme pins library [v1]", func(t *testing.T) {
		put(t, acme("v1"))
		list, err := admin.ExternalDependents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].ID != "acme" || list[0].ReviewDue || fmt.Sprint(list[0].Depends) != "[{library [v1]}]" {
			t.Fatalf("external dependents: %+v", list)
		}
		g := graph(t)
		if e := externalEdge(g); e == nil || !e.Met || e.ConsumerVersion != "" || fmt.Sprint(e.PresentLines) != "[v1]" {
			t.Fatalf("external edge: %+v (all: %+v)", e, g.DesiredEdges)
		}
		gw.WaitGauge(t, unmet, acmeUnmet, 0, wait)

		// Validation through the self-ingested GraphQL admin API: field
		// errors carry paths the UI form maps onto inputs.
		resp, err := gw.GraphQL(ctx, `mutation { admin { putExternalDependent(id: "acme", body: {kind: "customer", depends: [{service: "library", lines: ["stable"]}], reviewAt: "2027-06-30T00:00:00Z"}) { warnings } } }`, nil, harness.Bearer(gw.Token()))
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Errors) != 1 || !strings.Contains(fmt.Sprint(resp.Errors[0].Extensions["fields"]), "path:body.depends.0.lines") {
			t.Fatalf("GraphQL validation error: %+v", resp.Errors)
		}
	})

	var libB *harness.Fixture
	t.Run("2 v2 ships: v1 is held by Acme", func(t *testing.T) {
		libB = env.Fixture(gw, harness.FixtureOpts{Namespace: "library", Instance: "library-b", Versions: []string{"v2"}, Owner: "books", DrainOnSIGTERM: true})
		gw.WaitGauge(t, sunset, lib("v1"), 0, wait)

		g := graph(t)
		v1 := g.Node("library/v1")
		if v1 == nil || v1.Sunsetable || !strings.Contains(v1.SunsetReason, "external acme (Acme Corp) accepts library [v1]") {
			t.Fatalf("library/v1: %+v, want held by acme", v1)
		}
		// Nobody pins v2 and nobody calls it: sunset-able, by the
		// unchanged rule.
		if v2 := g.Node("library/v2"); v2 == nil || !v2.Sunsetable {
			t.Fatalf("library/v2: %+v", v2)
		}

		resp, err := admin.DrainInstance(ctx, "library-a", false)
		if err != nil {
			t.Fatal(err)
		}
		reasons := strings.Join(resp.GetReasons(), "; ")
		if harness.DrainStateName(resp.GetState()) != "blocked" || !strings.Contains(reasons, "external acme (Acme Corp) accepts library [v1]") {
			t.Fatalf("drain library-a: %s %q, want blocked naming acme", harness.DrainStateName(resp.GetState()), reasons)
		}
		admin.WaitDrainState("library-a", "blocked", wait)
		gw.WaitGauge(t, drains, harness.Labels{"namespace": "library", "version": "v1", "drain_state": "blocked"}, 1, wait)
	})

	t.Run("3 Acme moves to [v1,v2] then [v2]: v1 may go", func(t *testing.T) {
		put(t, acme("v1", "v2"))
		harness.Eventually(t, 90*time.Second, "library/v1 sunset-able in the 1m graph", func() error {
			n := graph(t).Node("library/v1")
			if n == nil || !n.Sunsetable {
				return fmt.Errorf("library/v1: %+v", n)
			}
			return nil
		})
		// The held drain proceeds by itself: v2 covers Acme.
		admin.WaitDrainState("library-a", "safe_to_stop", 30*time.Second)

		put(t, acme("v2"))
		g := graph(t)
		if n := g.Node("library/v1"); n == nil || !n.Sunsetable {
			t.Fatalf("library/v1 after acme [v2]: %+v", n)
		}
		if n := g.Node("library/v2"); n == nil || n.Sunsetable || !strings.Contains(n.SunsetReason, "external acme") {
			t.Fatalf("library/v2 after acme [v2]: %+v, want held by acme", n)
		}
		if code := libA.Proc.Stop(30 * time.Second); code != 0 {
			t.Fatalf("library-a exit %d", code)
		}
		if !strings.Contains(libA.Proc.Logs(), "drained and deregistered") {
			t.Error("library-a did not log a clean drain")
		}
		admin.WaitInstance("library", "v1", "library-a", "", wait)
		if _, err := libB.Echo(ctx, "v2", "after"); err != nil {
			t.Fatalf("v2 call: %v", err)
		}
	})

	t.Run("4 a pin with no ready line raises dependency_unmet", func(t *testing.T) {
		gw.WaitGauge(t, unmet, acmeUnmet, 0, wait)
		// Acme pins v1 again (a rollback on their side); no v1 is left.
		put(t, acme("v1"))
		gw.WaitGauge(t, unmet, acmeUnmet, 1, wait)
		if e := externalEdge(graph(t)); e == nil || e.Met {
			t.Fatalf("external edge: %+v, want unmet", e)
		}
		put(t, acme("v2"))
		gw.WaitGauge(t, unmet, acmeUnmet, 0, wait)
	})

	t.Run("5 the review date passes: nothing changes but the flag", func(t *testing.T) {
		x := acme("v2")
		x.ReviewAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		warnings, err := admin.PutExternalDependent(ctx, x)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(warnings, ";"), "review-due now") {
			t.Errorf("warnings=%v, want a past-reviewAt warning", warnings)
		}
		list, err := admin.ExternalDependents(ctx)
		if err != nil || len(list) != 1 || !list[0].ReviewDue {
			t.Fatalf("list: %+v %v, want acme review-due", list, err)
		}
		gw.WaitGauge(t, "declarations_review_due", nil, 1, wait)
		items, err := admin.Notifications(ctx)
		if err != nil || len(items) != 1 || items[0].Kind != "declaration_review_due" || items[0].Href != "/?select=ext:acme" {
			t.Fatalf("notifications: %+v %v", items, err)
		}
		// Still counts: the edge stays, and the last v2 instance stays
		// covered.
		if e := externalEdge(graph(t)); e == nil || !e.Met {
			t.Fatalf("external edge with a review-due pin: %+v", e)
		}
		resp, err := admin.DrainInstance(ctx, "library-b", false)
		if err != nil {
			t.Fatal(err)
		}
		if harness.DrainStateName(resp.GetState()) != "blocked" {
			t.Fatalf("drain library-b with a review-due pin: %s", harness.DrainStateName(resp.GetState()))
		}
		if _, err := admin.DrainInstance(ctx, "library-b", true); err != nil {
			t.Fatal(err)
		}

		next := time.Now().Add(180 * 24 * time.Hour).UTC().Format(time.RFC3339)
		r, err := admin.ReviewExternalDependent(ctx, "acme", next)
		if err != nil {
			t.Fatal(err)
		}
		if r.ReviewDue || r.ReviewAt != next || r.ReviewedBy != "admin" || r.ReviewedAt == "" || fmt.Sprint(r.Depends) != "[{library [v2]}]" {
			t.Fatalf("after review: %+v", r)
		}
		gw.WaitGauge(t, "declarations_review_due", nil, 0, wait)
		if items, err := admin.Notifications(ctx); err != nil || len(items) != 0 {
			t.Fatalf("notifications after review: %+v %v", items, err)
		}
	})

	t.Run("6 deleting the declaration", func(t *testing.T) {
		if err := admin.DeleteExternalDependent(ctx, "acme"); err != nil {
			t.Fatal(err)
		}
		if list, err := admin.ExternalDependents(ctx); err != nil || len(list) != 0 {
			t.Fatalf("after delete: %+v %v", list, err)
		}
		g := graph(t)
		if e := externalEdge(g); e != nil {
			t.Fatalf("edge after delete: %+v", e)
		}
		gw.WaitNoSeries(t, unmet, harness.Labels{"consumer_namespace": "acme"}, wait)
		// The only line left: never sunset-able, and with no
		// dependent its drain is not blocked.
		resp, err := admin.DrainInstance(ctx, "library-b", false)
		if err != nil {
			t.Fatal(err)
		}
		if s := harness.DrainStateName(resp.GetState()); s == "blocked" {
			t.Fatalf("drain library-b after delete: blocked %v", resp.GetReasons())
		}
		if _, err := admin.DrainInstance(ctx, "library-b", true); err != nil {
			t.Fatal(err)
		}
		if err := admin.DeleteExternalDependent(ctx, "acme"); err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("second delete: %v, want 404", err)
		}
	})
}
