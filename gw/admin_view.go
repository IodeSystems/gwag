package gateway

import (
	"context"
	"strings"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// adminView is what one admin read may show. Dev and admin get the
// full view. Anonymous callers get public services only:
//
//   - a (namespace, line) is listed when it is public (ServiceInfo.public);
//   - owner contact, instance addresses, drain reasons and rejected
//     joins are blanked;
//   - references to a namespace with no public line (depends entries,
//     declared graph edges) are dropped;
//   - a caller id that does not name a public service is reported as
//     hiddenCaller ("unknown" stays), so traffic totals stay true
//     without naming who called;
//   - external-dependent declarations are not shown at all.
type adminView struct {
	full  bool
	lines map[poolKey]bool // public lines
	ns    map[string]bool  // namespaces with a public line
	// cfgNS reports a namespace WithPublicServices makes public, so
	// references to it count as visible before it registers.
	cfgNS func(string) bool
}

// hiddenCaller replaces, in an anonymous view, a caller id that does
// not name a public service.
const hiddenCaller = "hidden"

// adminViewFor builds the view for ctx from a ListServices answer.
func (g *Gateway) adminViewFor(ctx context.Context, reg *cpv1.ListServicesResponse) adminView {
	v := adminView{full: adminFullView(ctx)}
	if v.full {
		return v
	}
	v.lines = map[poolKey]bool{}
	v.ns = map[string]bool{}
	v.cfgNS = func(ns string) bool {
		return !strings.HasPrefix(ns, "_") && (g.cfg.publicAll || g.cfg.publicNamespaces[ns])
	}
	for _, s := range reg.GetServices() {
		if s.GetPublic() {
			v.lines[poolKey{namespace: s.GetNamespace(), version: s.GetVersion()}] = true
			v.ns[s.GetNamespace()] = true
		}
	}
	return v
}

func (v adminView) line(ns, ver string) bool {
	return v.full || v.lines[poolKey{namespace: ns, version: ver}]
}

func (v adminView) namespace(ns string) bool {
	return v.full || v.ns[ns] || v.cfgNS(ns)
}

// caller is the label for caller id c (see adminView).
func (v adminView) caller(c string) string {
	if v.full || c == "unknown" {
		return c
	}
	if i := strings.IndexByte(c, '/'); i > 0 {
		if v.lines[poolKey{namespace: c[:i], version: c[i+1:]}] || v.cfgNS(c[:i]) {
			return c
		}
		return hiddenCaller
	}
	if v.namespace(c) {
		return c
	}
	return hiddenCaller
}

// depends drops entries naming a namespace with no public line.
func (v adminView) depends(in []dependencyInfo) []dependencyInfo {
	if v.full {
		return in
	}
	out := []dependencyInfo{}
	for _, d := range in {
		if v.namespace(d.Service) {
			out = append(out, d)
		}
	}
	return out
}

func (v adminView) owner(o *ownerInfo) *ownerInfo {
	if v.full || o == nil {
		return o
	}
	return &ownerInfo{Team: o.Team}
}

// service scrubs one listServices row for the view.
func (v adminView) service(info *serviceInfo) {
	if v.full {
		return
	}
	info.Owner = v.owner(info.Owner)
	info.Depends = v.depends(info.Depends)
	info.Subscribes = []subscriptionInfo{}
	info.RejectedJoins = nil
	for i := range info.Drains {
		info.Drains[i].Addr = ""
		info.Drains[i].Reasons = []string{}
	}
	for i := range info.Instances {
		inst := &info.Instances[i]
		inst.Addr = ""
		deps := []dependencyStatusInfo{}
		for _, d := range inst.Depends {
			if v.namespace(d.Service) {
				deps = append(deps, d)
			}
		}
		inst.Depends = deps
	}
}

// mergeMethodStats folds rows that share (method, caller) — several
// hidden callers become one row. Counts and throughput add;
// percentiles take the max, as servicesStats does.
func mergeMethodStats(rows []methodStatsOut) []methodStatsOut {
	type key struct{ method, caller string }
	idx := map[key]int{}
	out := rows[:0]
	for _, r := range rows {
		k := key{r.Method, r.Caller}
		if i, ok := idx[k]; ok {
			m := &out[i]
			m.Count += r.Count
			m.OkCount += r.OkCount
			m.Throughput += r.Throughput
			m.P50Millis = max(m.P50Millis, r.P50Millis)
			m.P95Millis = max(m.P95Millis, r.P95Millis)
			m.P99Millis = max(m.P99Millis, r.P99Millis)
			continue
		}
		idx[k] = len(out)
		out = append(out, r)
	}
	return out
}

// mergeCallerStats is mergeMethodStats for one method's callers.
func mergeCallerStats(rows []callerStatsRow) []callerStatsRow {
	idx := map[string]int{}
	out := rows[:0]
	for _, r := range rows {
		if i, ok := idx[r.Caller]; ok {
			m := &out[i]
			m.Count += r.Count
			m.OkCount += r.OkCount
			m.Throughput += r.Throughput
			m.P50Millis = max(m.P50Millis, r.P50Millis)
			m.P95Millis = max(m.P95Millis, r.P95Millis)
			m.P99Millis = max(m.P99Millis, r.P99Millis)
			continue
		}
		idx[r.Caller] = len(out)
		out = append(out, r)
	}
	return out
}
