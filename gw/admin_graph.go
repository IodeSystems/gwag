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
// Channel edges (edgeKind "channel"): one per declared subscription
// (ServiceBinding.subscribes), from the subscriber line to the line
// that declares the channel (gateDepsLocked). channel and group say
// which; provider and lines are empty while no line declares it. They
// count like RPC edges for met / unmet and the sunset rule.
//
// External dependents (external_dependents.go): each declaration
// adds one desired edge per pinned service, with consumerKind
// "external", consumerNamespace = its id and consumerVersion "". They
// count in the sunset rule like any other dependent, review-due ones
// included. Every declaration is listed in externalDependents.
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
	Verification   string   `json:"verification" enum:"none,pending,green,red" doc:"none: the instance declares no verify. pending or red: not routable (state waiting)."`
	VerifyReasons  []string `json:"verifyReasons"`
}

type graphNode struct {
	ID             string          `json:"id" doc:"namespace/version"`
	Namespace      string          `json:"namespace"`
	Version        string          `json:"version"`
	Kind           string          `json:"kind" doc:"proto, openapi, graphql, internalproto, mcp; empty when no instance is in the pool yet."`
	Public         bool            `json:"public" doc:"Shown to admin readers without a token."`
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
	EdgeKind          string   `json:"edgeKind" enum:"rpc,channel" doc:"rpc: a declared dependency (depends); channel: a declared subscription (subscribes) to an event channel the provider declares."`
	Channel           string   `json:"channel" doc:"Channel key of a channel edge, e.g. orders.v1.OrderEvents; empty for rpc edges."`
	Group             string   `json:"group" doc:"Consumer group of a channel edge; empty for rpc edges."`
	ConsumerNamespace string   `json:"consumerNamespace"`
	ConsumerVersion   string   `json:"consumerVersion"`
	Provider          string   `json:"provider" doc:"Provider namespace. Empty on a channel edge while no line declares the channel."`
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
	Caller            string             `json:"caller" doc:"Caller id. Without a token, a caller that is not a public service reads \"hidden\"."`
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

		ExternalDependents []externalDependentInfo `json:"externalDependents" doc:"Declared external dependents. Review-due ones (reviewDue) still add edges."`
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
		view := g.adminViewFor(ctx, reg)
		out, err := g.buildDependencyGraph(reg, in.Window, window, view)
		if err != nil {
			return nil, err
		}
		view.graph(out)
		return out, nil
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

// buildDependencyGraph builds the whole graph; view only changes how
// sunset reasons name consumers it hides (adminView.graph removes the
// rest afterwards), so the sunset rule counts every dependent.
func (g *Gateway) buildDependencyGraph(reg *cpv1.ListServicesResponse, windowName string, window time.Duration, view adminView) (*dependencyGraphOut, error) {
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
	lineSubs := map[poolKey][]*cpv1.Subscription{}
	type edgeKey struct {
		external bool
		consumer poolKey
		provider string
		lines    string
		channel  string
		group    string
	}
	edges := map[edgeKey]*graphDesiredEdge{}
	var edgeOrder []edgeKey
	addChannelEdge := func(consumer poolKey, provider string, lines []string, channel, group string, fromInstance bool) {
		sorted := append([]string(nil), lines...)
		sort.Strings(sorted)
		k := edgeKey{false, consumer, provider, strings.Join(sorted, ","), channel, group}
		e := edges[k]
		if e == nil {
			edgeKind := "rpc"
			if channel != "" {
				edgeKind = "channel"
			}
			e = &graphDesiredEdge{
				ConsumerKind:      "service",
				EdgeKind:          edgeKind,
				Channel:           channel,
				Group:             group,
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
	addEdge := func(consumer poolKey, provider string, lines []string, fromInstance, external bool) {
		sorted := append([]string(nil), lines...)
		sort.Strings(sorted)
		k := edgeKey{external: external, consumer: consumer, provider: provider, lines: strings.Join(sorted, ",")}
		e := edges[k]
		if e == nil {
			kind := "service"
			if external {
				kind = "external"
			}
			e = &graphDesiredEdge{
				ConsumerKind:      kind,
				EdgeKind:          "rpc",
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
			Public:       s.GetPublic(),
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
				Verification:   adminVerifyState(inst.GetVerification()),
				VerifyReasons:  append([]string{}, inst.GetVerifyReasons()...),
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
				if d.GetChannel() != "" {
					addChannelEdge(k, d.GetService(), d.GetLines(), d.GetChannel(), d.GetGroup(), true)
					continue
				}
				addEdge(k, d.GetService(), d.GetLines(), true, false)
			}
		}
		if len(s.GetInstances()) == 0 {
			for _, d := range s.GetDepends() {
				addEdge(k, d.GetService(), d.GetLines(), false, false)
			}
			if len(s.GetSubscribes()) > 0 {
				lineSubs[k] = s.GetSubscribes()
			}
		}
	}

	// Line-manifest subscriptions (no instance listed): resolve the
	// declaring line the way the start gate does.
	if len(lineSubs) > 0 {
		g.mu.Lock()
		for k, subs := range lineSubs {
			m := serviceManifest{}
			for _, sub := range subs {
				m.Subscribes = append(m.Subscribes, manifestSubscription{Channel: sub.GetChannel(), Group: sub.GetGroup()})
			}
			for _, d := range g.gateDepsLocked(m, k) {
				addChannelEdge(k, d.Service, d.Lines, d.Channel, d.Group, false)
			}
		}
		g.mu.Unlock()
	}

	// External dependents: every declaration counts, review-due or not.
	extLabel := map[string]string{}
	for _, x := range g.externals.all() {
		out.Body.ExternalDependents = append(out.Body.ExternalDependents, adminExternalDependent(x, now))
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
				switch {
				case ek.external && !view.full:
					who = "an external dependent"
				case ek.external:
					who = extLabel[e.ConsumerNamespace]
				case !view.line(e.ConsumerNamespace, e.ConsumerVersion):
					who = "a hidden service"
				}
				if e.Channel != "" {
					reasons = append(reasons, fmt.Sprintf("%s subscribes to %s (group %s)", who, e.Channel, e.Group))
				} else {
					reasons = append(reasons, fmt.Sprintf("%s accepts %s [%s]: no other ready line", who, e.Provider, strings.Join(e.Lines, ", ")))
				}
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
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Channel < b.Channel
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

// graph removes what an anonymous view may not show from a built
// graph (see adminView): non-public nodes; declared edges with a
// hidden end (dropped, not anonymised: the provider's sunset reason
// still says "a hidden service accepts ..."); external dependents and
// their edges; owner contact, instance addresses and drain reasons.
// Traffic from callers that are not public services is kept and
// folded into one hiddenCaller edge per (namespace, version), so
// node traffic stays true.
func (v adminView) graph(out *dependencyGraphOut) {
	if v.full {
		return
	}
	nodes := out.Body.Nodes[:0]
	for _, n := range out.Body.Nodes {
		if !v.line(n.Namespace, n.Version) {
			continue
		}
		n.Owner = v.owner(n.Owner)
		for i := range n.Instances {
			n.Instances[i].Addr = ""
			n.Instances[i].DrainReasons = []string{}
		}
		nodes = append(nodes, n)
	}
	out.Body.Nodes = nodes

	desired := out.Body.DesiredEdges[:0]
	for _, e := range out.Body.DesiredEdges {
		if e.ConsumerKind != "service" || !v.line(e.ConsumerNamespace, e.ConsumerVersion) || !v.namespace(e.Provider) {
			continue
		}
		desired = append(desired, e)
	}
	out.Body.DesiredEdges = desired

	type key struct{ caller, ns, ver string }
	idx := map[key]int{}
	active := []graphActiveEdge{}
	for _, e := range out.Body.ActiveEdges {
		if !v.line(e.Namespace, e.Version) {
			continue
		}
		if c := v.caller(e.Caller); c != e.Caller {
			e.Caller, e.ConsumerNamespace, e.ConsumerVersion = c, "", ""
		}
		k := key{e.Caller, e.Namespace, e.Version}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(active)
			active = append(active, e)
			continue
		}
		a := &active[i]
		a.Count += e.Count
		a.OkCount += e.OkCount
		a.LastSeenUnixMs = max(a.LastSeenUnixMs, e.LastSeenUnixMs)
		a.Methods = mergeGraphMethods(append(a.Methods, e.Methods...))
	}
	out.Body.ActiveEdges = active
	out.Body.ExternalDependents = []externalDependentInfo{}
}

func mergeGraphMethods(in []graphMethodCount) []graphMethodCount {
	idx := map[string]int{}
	out := []graphMethodCount{}
	for _, m := range in {
		if i, ok := idx[m.Method]; ok {
			out[i].Count += m.Count
			out[i].OkCount += m.OkCount
			continue
		}
		idx[m.Method] = len(out)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Method < out[j].Method
	})
	return out
}
