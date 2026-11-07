package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Dependency graph: one read for the admin UI's main screen. Joins
// the manifests (listServices), the start gate's ready set, drain
// state (listInstances) and usage (usageEdges) into nodes and edges.
//
// Nodes: one per registered (namespace, line).
//
// Desired edges: from each instance's declared depends (or the line's
// manifest when no control-plane instance is listed, e.g. boot-time
// registrations). One edge per (consumer line, provider namespace,
// accepted lines); presentLines are the accepted lines with a ready
// (non-draining) instance now; met = presentLines is not empty.
//
// Active edges: from the per-node usage rings, one per (caller,
// namespace, line) with traffic in the window. The caller is mapped to
// a consumer node by this rule:
//   - caller "ns/line" where (ns, line) is a registered line → that
//     line (consumerNamespace + consumerVersion);
//   - caller equal to a registered namespace → that namespace
//     (consumerNamespace only; the line is unknown);
//   - anything else (incl. "unknown") → an external caller,
//     consumerNamespace "".
//
// External dependents (external_dependents.go): each declaration
// that has not expired adds one desired edge per pinned service, with
// consumerKind "external", consumerNamespace = its id and
// consumerVersion "". They count in the sunset rule like any other
// dependent. Every declaration, expired ones included, is listed in
// externalDependents.
//
// Sunset-able (design): provider line L may go when every dependent
// that accepts L also accepts another line with a ready instance, and
// L had no traffic in the window. Uses the same ready set as the start
// gate and drain coverage (readyOwnersLocked). The only line of a
// namespace is never sunset-able: with no dependents the rule holds
// trivially, and the flag would then mark every idle leaf service.
//
// Traffic is what THIS node dispatched (scope "node"); in a cluster
// each node reports its own share.

type dependencyGraphIn struct {
	Window string `query:"window" default:"1h" doc:"Usage window: 1m, 1h or 24h."`
}

type graphInstance struct {
	RegistrationID string   `json:"registrationId"`
	InstanceID     string   `json:"instanceId,omitempty"`
	Addr           string   `json:"addr"`
	State          string   `json:"state" enum:"ready,waiting,degraded"`
	DrainState     string   `json:"drainState" enum:"serving,draining,blocked,safe_to_stop"`
	DrainReasons   []string `json:"drainReasons"`
}

type graphNode struct {
	ID             string          `json:"id" doc:"namespace/version"`
	Namespace      string          `json:"namespace"`
	Version        string          `json:"version"`
	Kind           string          `json:"kind" doc:"proto, openapi, graphql, internalproto, mcp; empty when no instance is in the pool yet."`
	Owner          *ownerInfo      `json:"owner,omitempty"`
	ReplicaCount   uint32          `json:"replicaCount"`
	ReadyCount     int             `json:"readyCount"`
	WaitingCount   int             `json:"waitingCount"`
	DegradedCount  int             `json:"degradedCount"`
	DrainingCount  int             `json:"drainingCount" doc:"Instances with a drain requested (draining, blocked or safe_to_stop)."`
	TrafficCount   uint64          `json:"trafficCount" doc:"Calls in the window, this node."`
	LastSeenUnixMs int64           `json:"lastSeenUnixMs"`
	Sunsetable     bool            `json:"sunsetable"`
	SunsetReason   string          `json:"sunsetReason"`
	Instances      []graphInstance `json:"instances"`
}

type graphDesiredEdge struct {
	ConsumerKind      string   `json:"consumerKind" enum:"service,external" doc:"service: a registered line; external: a declared external dependent (consumerNamespace is its id, consumerVersion is empty)."`
	ConsumerNamespace string   `json:"consumerNamespace"`
	ConsumerVersion   string   `json:"consumerVersion"`
	Provider          string   `json:"provider" doc:"Provider namespace."`
	Lines             []string `json:"lines" doc:"Accepted lines."`
	PresentLines      []string `json:"presentLines" doc:"Accepted lines with a ready instance."`
	Met               bool     `json:"met"`
	InstanceCount     int      `json:"instanceCount" doc:"Consumer instances that declare this dependency (0 = line manifest only)."`
}

type graphMethodCount struct {
	Method  string `json:"method"`
	Count   uint64 `json:"count"`
	OkCount uint64 `json:"okCount"`
}

type graphActiveEdge struct {
	Caller            string             `json:"caller"`
	ConsumerNamespace string             `json:"consumerNamespace" doc:"Registered namespace the caller maps to; empty = external caller."`
	ConsumerVersion   string             `json:"consumerVersion" doc:"Set only when the caller id is \"ns/line\"."`
	Namespace         string             `json:"namespace"`
	Version           string             `json:"version"`
	Count             uint64             `json:"count"`
	OkCount           uint64             `json:"okCount"`
	LastSeenUnixMs    int64              `json:"lastSeenUnixMs"`
	Methods           []graphMethodCount `json:"methods"`
}

type dependencyGraphOut struct {
	Body struct {
		Environment  string             `json:"environment"`
		Window       string             `json:"window"`
		Scope        string             `json:"scope" enum:"node" doc:"Usage is per gateway node, in memory."`
		CallerSource string             `json:"callerSource" enum:"header,extractor,none" doc:"header: caller ids come from a request header and can be forged; extractor: a configured CallerIDExtractor; none: every caller is \"unknown\"."`
		Nodes        []graphNode        `json:"nodes"`
		DesiredEdges []graphDesiredEdge `json:"desiredEdges"`
		ActiveEdges  []graphActiveEdge  `json:"activeEdges"`

		ExternalDependents []externalDependentInfo `json:"externalDependents" doc:"Declared external dependents, expired ones included (expired ones add no edges)."`
	}
}

func registerDependencyGraph(api huma.API, g *Gateway, cp *controlPlane) {
	huma.Register(api, huma.Operation{
		OperationID: "dependencyGraph",
		Method:      http.MethodGet,
		Path:        "/admin/graph",
		Summary:     "Dependency graph: one node per (namespace, line) with instance states, owner and sunset-able flag; desired edges from declared depends (met / unmet); active edges from observed traffic in the window. Traffic is this node's only.",
	}, func(ctx context.Context, in *dependencyGraphIn) (*dependencyGraphOut, error) {
		window, err := parseStatsWindow(in.Window)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		reg, err := cp.ListServices(ctx, &cpv1.ListServicesRequest{})
		if err != nil {
			return nil, err
		}
		return g.buildDependencyGraph(reg, in.Window, window)
	})
}

func (g *Gateway) callerSource() string {
	switch {
	case g.cfg.callerIDExtractor != nil && g.cfg.callerIDPublic && g.cfg.callerIDDelegated == nil:
		return "header"
	case g.cfg.callerIDExtractor != nil:
		return "extractor"
	case len(g.cfg.callerHeaders) > 0:
		return "header"
	}
	return "none"
}

func (g *Gateway) buildDependencyGraph(reg *cpv1.ListServicesResponse, windowName string, window time.Duration) (*dependencyGraphOut, error) {
	out := &dependencyGraphOut{}
	out.Body.Environment = reg.GetEnvironment()
	out.Body.Window = windowName
	out.Body.Scope = "node"
	out.Body.CallerSource = g.callerSource()
	out.Body.Nodes = []graphNode{}
	out.Body.DesiredEdges = []graphDesiredEdge{}
	out.Body.ActiveEdges = []graphActiveEdge{}
	out.Body.ExternalDependents = []externalDependentInfo{}
	now := nowFunc()

	// Drain state per registration.
	drains := map[string]instanceDrainStatus{}
	for _, st := range g.listInstanceDrains("") {
		drains[st.RegID] = st
	}

	nodes := map[poolKey]*graphNode{}
	namespaces := map[string]bool{}
	type edgeKey struct {
		external bool
		consumer poolKey
		provider string
		lines    string
	}
	edges := map[edgeKey]*graphDesiredEdge{}
	var edgeOrder []edgeKey
	addEdge := func(consumer poolKey, provider string, lines []string, fromInstance, external bool) {
		sorted := append([]string(nil), lines...)
		sort.Strings(sorted)
		k := edgeKey{external, consumer, provider, strings.Join(sorted, ",")}
		e := edges[k]
		if e == nil {
			kind := "service"
			if external {
				kind = "external"
			}
			e = &graphDesiredEdge{
				ConsumerKind:      kind,
				ConsumerNamespace: consumer.namespace,
				ConsumerVersion:   consumer.version,
				Provider:          provider,
				Lines:             append([]string{}, lines...),
			}
			edges[k] = e
			edgeOrder = append(edgeOrder, k)
		}
		if fromInstance {
			e.InstanceCount++
		}
	}

	for _, s := range reg.GetServices() {
		k := poolKey{namespace: s.GetNamespace(), version: s.GetVersion()}
		namespaces[k.namespace] = true
		n := &graphNode{
			ID:           k.namespace + "/" + k.version,
			Namespace:    k.namespace,
			Version:      k.version,
			Owner:        adminOwner(s.GetOwner()),
			ReplicaCount: s.GetReplicaCount(),
			Instances:    []graphInstance{},
		}
		nodes[k] = n
		for _, inst := range s.GetInstances() {
			state := adminInstanceState(inst.GetState())
			switch state {
			case "waiting":
				n.WaitingCount++
			case "degraded":
				n.DegradedCount++
			default:
				n.ReadyCount++
			}
			gi := graphInstance{
				RegistrationID: inst.GetRegistrationId(),
				InstanceID:     inst.GetInstanceId(),
				Addr:           inst.GetAddr(),
				State:          state,
				DrainState:     drainStateServing,
				DrainReasons:   []string{},
			}
			if st, ok := drains[inst.GetRegistrationId()]; ok {
				gi.DrainState = st.State
				gi.DrainReasons = append(gi.DrainReasons, st.Reasons...)
				if st.State != drainStateServing {
					n.DrainingCount++
				}
			}
			n.Instances = append(n.Instances, gi)
			for _, d := range inst.GetDepends() {
				addEdge(k, d.GetService(), d.GetLines(), true, false)
			}
		}
		if len(s.GetInstances()) == 0 {
			for _, d := range s.GetDepends() {
				addEdge(k, d.GetService(), d.GetLines(), false, false)
			}
		}
	}

	// External dependents: edges for the ones that count now.
	extLabel := map[string]string{}
	for _, x := range g.externals.all() {
		out.Body.ExternalDependents = append(out.Body.ExternalDependents, adminExternalDependent(x, now))
		if x.expired(now) {
			continue
		}
		extLabel[x.ID] = x.label()
		for _, d := range x.Depends {
			addEdge(poolKey{namespace: x.ID}, d.Service, d.Lines, false, true)
		}
	}

	// Kind, present lines and the ready set, under one g.mu hold.
	ready := map[poolKey]int{}
	readyCount := func(ns, line string) int {
		k := poolKey{namespace: ns, version: line}
		if c, ok := ready[k]; ok {
			return c
		}
		c := g.readyInstancesLocked(ns, line)
		ready[k] = c
		return c
	}
	g.mu.Lock()
	for k, n := range nodes {
		if s := g.slots[k]; s != nil {
			n.Kind = s.kind.String()
		}
	}
	for _, k := range edgeOrder {
		e := edges[k]
		e.PresentLines = []string{}
		for _, l := range e.Lines {
			if readyCount(e.Provider, l) > 0 {
				e.PresentLines = append(e.PresentLines, l)
			}
		}
		e.Met = len(e.PresentLines) > 0
	}
	g.mu.Unlock()

	// Active edges.
	type activeKey struct{ caller, ns, ver string }
	active := map[activeKey]*graphActiveEdge{}
	for _, r := range g.snapshot(window, nowFunc()) {
		target := poolKey{namespace: r.Namespace, version: r.Version}
		n := nodes[target]
		if n == nil {
			continue // internal or no longer registered
		}
		n.TrafficCount += r.Count
		if r.LastSeenUnixMs > n.LastSeenUnixMs {
			n.LastSeenUnixMs = r.LastSeenUnixMs
		}
		ak := activeKey{r.Caller, r.Namespace, r.Version}
		e := active[ak]
		if e == nil {
			e = &graphActiveEdge{Caller: r.Caller, Namespace: r.Namespace, Version: r.Version, Methods: []graphMethodCount{}}
			e.ConsumerNamespace, e.ConsumerVersion = mapCaller(r.Caller, nodes, namespaces)
			active[ak] = e
		}
		e.Count += r.Count
		e.OkCount += r.OkCount
		if r.LastSeenUnixMs > e.LastSeenUnixMs {
			e.LastSeenUnixMs = r.LastSeenUnixMs
		}
		e.Methods = append(e.Methods, graphMethodCount{Method: r.Method, Count: r.Count, OkCount: r.OkCount})
	}

	// Sunset-able.
	nsLines := map[string]int{}
	for k := range nodes {
		nsLines[k.namespace]++
	}
	for k, n := range nodes {
		if nsLines[k.namespace] < 2 {
			n.SunsetReason = "only line of " + k.namespace
			continue
		}
		var reasons []string
		for _, ek := range edgeOrder {
			e := edges[ek]
			if e.Provider != k.namespace || !containsString(e.Lines, k.version) {
				continue
			}
			covered := false
			for _, l := range e.Lines {
				if l != k.version && ready[poolKey{namespace: e.Provider, version: l}] > 0 {
					covered = true
					break
				}
			}
			if !covered {
				who := e.ConsumerNamespace + "/" + e.ConsumerVersion
				if ek.external {
					who = extLabel[e.ConsumerNamespace]
				}
				reasons = append(reasons, fmt.Sprintf("%s accepts %s [%s]: no other ready line", who, e.Provider, strings.Join(e.Lines, ", ")))
			}
		}
		if n.TrafficCount > 0 {
			reasons = append(reasons, fmt.Sprintf("%d calls in the last %s on this node", n.TrafficCount, windowName))
		}
		if len(reasons) == 0 {
			n.Sunsetable = true
			n.SunsetReason = fmt.Sprintf("every dependent has another ready line; no traffic in the last %s on this node", windowName)
		} else {
			sort.Strings(reasons)
			n.SunsetReason = strings.Join(reasons, "; ")
		}
	}

	for _, n := range nodes {
		out.Body.Nodes = append(out.Body.Nodes, *n)
	}
	sort.Slice(out.Body.Nodes, func(i, j int) bool {
		a, b := out.Body.Nodes[i], out.Body.Nodes[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Version < b.Version
	})
	for _, k := range edgeOrder {
		out.Body.DesiredEdges = append(out.Body.DesiredEdges, *edges[k])
	}
	sort.SliceStable(out.Body.DesiredEdges, func(i, j int) bool {
		a, b := out.Body.DesiredEdges[i], out.Body.DesiredEdges[j]
		if a.ConsumerKind != b.ConsumerKind {
			return a.ConsumerKind > b.ConsumerKind // service before external
		}
		if a.ConsumerNamespace != b.ConsumerNamespace {
			return a.ConsumerNamespace < b.ConsumerNamespace
		}
		if a.ConsumerVersion != b.ConsumerVersion {
			return a.ConsumerVersion < b.ConsumerVersion
		}
		return a.Provider < b.Provider
	})
	for _, e := range active {
		sort.Slice(e.Methods, func(i, j int) bool {
			if e.Methods[i].Count != e.Methods[j].Count {
				return e.Methods[i].Count > e.Methods[j].Count
			}
			return e.Methods[i].Method < e.Methods[j].Method
		})
		out.Body.ActiveEdges = append(out.Body.ActiveEdges, *e)
	}
	sort.Slice(out.Body.ActiveEdges, func(i, j int) bool {
		a, b := out.Body.ActiveEdges[i], out.Body.ActiveEdges[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Caller != b.Caller {
			return a.Caller < b.Caller
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Version < b.Version
	})
	return out, nil
}

// mapCaller applies the caller → consumer rule (see file comment).
func mapCaller(caller string, nodes map[poolKey]*graphNode, namespaces map[string]bool) (ns, ver string) {
	if i := strings.IndexByte(caller, '/'); i > 0 {
		k := poolKey{namespace: caller[:i], version: caller[i+1:]}
		if nodes[k] != nil {
			return k.namespace, k.version
		}
	}
	if namespaces[caller] {
		return caller, ""
	}
	return "", ""
}
