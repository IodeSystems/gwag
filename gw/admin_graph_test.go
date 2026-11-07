package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

type graphResp struct {
	Environment  string `json:"environment"`
	Window       string `json:"window"`
	Scope        string `json:"scope"`
	CallerSource string `json:"callerSource"`
	Nodes        []struct {
		ID, Namespace, Version, Kind string
		ReadyCount, WaitingCount     int
		TrafficCount                 uint64
		Sunsetable                   bool
		SunsetReason                 string
	} `json:"nodes"`
	DesiredEdges []struct {
		ConsumerNamespace, ConsumerVersion, Provider string
		Lines, PresentLines                          []string
		Met                                          bool
		InstanceCount                                int
	} `json:"desiredEdges"`
	ActiveEdges []struct {
		Caller, ConsumerNamespace, ConsumerVersion, Namespace, Version string
		Count                                                         uint64
		Methods                                                       []struct {
			Method string
			Count  uint64
		}
	} `json:"activeEdges"`
}

func getGraph(t *testing.T, g *Gateway, q string) graphResp {
	t.Helper()
	rr := httptest.NewRecorder()
	newAdminRouter(t, g).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/graph"+q, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out graphResp
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r graphResp) node(t *testing.T, id string) (n struct {
	ID, Namespace, Version, Kind string
	ReadyCount, WaitingCount     int
	TrafficCount                 uint64
	Sunsetable                   bool
	SunsetReason                 string
}) {
	t.Helper()
	for _, x := range r.Nodes {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("no node %s in %+v", id, r.Nodes)
	return
}

// consumerReq registers namespace ns (library proto) depending on
// greeter with the given lines. Never dialled.
func consumerReq(t *testing.T, ns string, greeterLines ...string) *cpv1.RegisterRequest {
	req := libraryConsumerReq(t, greeterLines...)
	req.InstanceId = ns + "-0"
	req.Services[0].Namespace = ns
	return req
}

func recordCall(g *Gateway, ns, ver, method, caller string) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Caller-Service", caller)
	g.cfg.metrics.RecordDispatch(withHTTPRequest(context.Background(), req), ns, ver, method, time.Millisecond, nil)
}

func TestDependencyGraph_DesiredEdges(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithEnvironment("staging"))
	t.Cleanup(g.Close)
	registerOn(t, g, greeterReq(t, addr, "greeter-v1", "v1"))
	registerOn(t, g, consumerReq(t, "library", "v1", "v2")) // any line: v1 present
	// Depends on a namespace nobody serves: waiting, unmet.
	mustRegister(t, g, "127.0.0.1:1", dependsOn(libraryBinding(t, "v1"), "ghost", "v1"))

	out := getGraph(t, g, "")
	if out.Environment != "staging" || out.Window != "1h" || out.Scope != "node" || out.CallerSource != "none" {
		t.Errorf("header=%q %q %q %q", out.Environment, out.Window, out.Scope, out.CallerSource)
	}
	if n := out.node(t, "greeter/v1"); n.Kind != "proto" || n.ReadyCount != 1 {
		t.Errorf("greeter/v1=%+v", n)
	}
	if len(out.DesiredEdges) != 2 {
		t.Fatalf("desired=%+v, want 2", out.DesiredEdges)
	}
	met, unmet := out.DesiredEdges[0], out.DesiredEdges[1]
	if met.Provider != "greeter" {
		met, unmet = unmet, met
	}
	if met.ConsumerNamespace != "library" || met.ConsumerVersion != "v1" || !met.Met ||
		!reflect.DeepEqual(met.Lines, []string{"v1", "v2"}) || !reflect.DeepEqual(met.PresentLines, []string{"v1"}) || met.InstanceCount != 1 {
		t.Errorf("met edge=%+v", met)
	}
	if unmet.Provider != "ghost" || unmet.Met || len(unmet.PresentLines) != 0 {
		t.Errorf("unmet edge=%+v", unmet)
	}
	if n := out.node(t, "library/v1"); n.WaitingCount != 1 || n.ReadyCount != 1 {
		t.Errorf("library/v1=%+v, want 1 ready + 1 waiting", n)
	}
}

func TestDependencyGraph_Sunsetable(t *testing.T) {
	_, a1 := startGreeterServer(t)
	_, a2 := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithCallerHeaders("X-Caller-Service"))
	t.Cleanup(g.Close)
	registerOn(t, g, greeterReq(t, a1, "greeter-v1", "v1"))
	registerOn(t, g, greeterReq(t, a2, "greeter-v2", "v2"))
	registerOn(t, g, consumerReq(t, "library", "v1", "v2"))

	// Only [v1, v2] dependents, no traffic: v1 may go.
	if n := getGraph(t, g, "").node(t, "greeter/v1"); !n.Sunsetable {
		t.Errorf("greeter/v1 not sunsetable: %s", n.SunsetReason)
	}

	// Traffic on v1 in the window: not sunsetable. The 1m window
	// still sees it; usage is per window.
	recordCall(g, "greeter", "v1", "Hello", "library")
	n := getGraph(t, g, "?window=1m").node(t, "greeter/v1")
	if n.Sunsetable || n.TrafficCount != 1 || !strings.Contains(n.SunsetReason, "1 calls") {
		t.Errorf("greeter/v1 with traffic=%+v", n)
	}
}

func TestDependencyGraph_SingleLineDependentBlocksSunset(t *testing.T) {
	_, a1 := startGreeterServer(t)
	_, a2 := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	registerOn(t, g, greeterReq(t, a1, "greeter-v1", "v1"))
	registerOn(t, g, greeterReq(t, a2, "greeter-v2", "v2"))
	registerOn(t, g, consumerReq(t, "library", "v1", "v2"))
	registerOn(t, g, consumerReq(t, "audit", "v1"))

	out := getGraph(t, g, "")
	v1 := out.node(t, "greeter/v1")
	if v1.Sunsetable || !strings.Contains(v1.SunsetReason, "audit/v1 accepts greeter [v1]") {
		t.Errorf("greeter/v1=%+v, want blocked by audit", v1)
	}
	// v2: library also accepts v1 (ready); audit does not accept v2.
	if v2 := out.node(t, "greeter/v2"); !v2.Sunsetable {
		t.Errorf("greeter/v2 not sunsetable: %s", v2.SunsetReason)
	}
	// The only line of a namespace: never sunsetable.
	if a := out.node(t, "audit/v1"); a.Sunsetable || a.SunsetReason != "only line of audit" {
		t.Errorf("audit/v1=%+v", a)
	}
}

func TestDependencyGraph_ActiveEdges(t *testing.T) {
	_, addr := startGreeterServer(t)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithCallerHeaders("X-Caller-Service"))
	t.Cleanup(g.Close)
	registerOn(t, g, greeterReq(t, addr, "greeter-v1", "v1"))
	registerOn(t, g, consumerReq(t, "library", "v1"))

	recordCall(g, "greeter", "v1", "Hello", "library")
	recordCall(g, "greeter", "v1", "Hello", "library")
	recordCall(g, "greeter", "v1", "Bye", "library")
	recordCall(g, "greeter", "v1", "Hello", "library/v1")
	recordCall(g, "greeter", "v1", "Hello", "browser")

	out := getGraph(t, g, "?window=1m")
	if out.CallerSource != "header" {
		t.Errorf("callerSource=%q, want header", out.CallerSource)
	}
	if len(out.ActiveEdges) != 3 {
		t.Fatalf("active=%+v, want 3", out.ActiveEdges)
	}
	top := out.ActiveEdges[0]
	if top.Caller != "library" || top.ConsumerNamespace != "library" || top.ConsumerVersion != "" ||
		top.Namespace != "greeter" || top.Version != "v1" || top.Count != 3 ||
		len(top.Methods) != 2 || top.Methods[0].Method != "Hello" || top.Methods[0].Count != 2 {
		t.Errorf("top=%+v", top)
	}
	for _, e := range out.ActiveEdges[1:] {
		switch e.Caller {
		case "library/v1":
			if e.ConsumerNamespace != "library" || e.ConsumerVersion != "v1" {
				t.Errorf("library/v1 edge=%+v", e)
			}
		case "browser":
			if e.ConsumerNamespace != "" {
				t.Errorf("browser edge=%+v, want external", e)
			}
		default:
			t.Errorf("unexpected edge %+v", e)
		}
	}
	if n := out.node(t, "greeter/v1"); n.TrafficCount != 5 {
		t.Errorf("greeter/v1 traffic=%d, want 5", n.TrafficCount)
	}
}

func TestDependencyGraph_CallerSource(t *testing.T) {
	for _, tc := range []struct {
		opt  Option
		want string
	}{
		{WithCallerIDPublic(), "header"},
		{WithCallerIDExtractor(func(context.Context) (string, error) { return "x", nil }), "extractor"},
		{WithCallerIDHMAC(CallerIDHMACOptions{Secret: []byte("s")}), "extractor"},
	} {
		g := New(WithoutMetrics(), WithoutBackpressure(), tc.opt)
		if got := g.callerSource(); got != tc.want {
			t.Errorf("callerSource=%q, want %q", got, tc.want)
		}
		g.Close()
	}
}
