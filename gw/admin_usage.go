package gateway

import (
	"context"
	"net/http"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Active dependencies by method: who calls which (namespace, version,
// method) over a window, for every registered service. Reads the same
// per-(ns, ver, method, caller) rings that feed deprecatedStats, so
// the caller identity is whatever WithCallerIDExtractor /
// WithCallerHeaders resolves ("unknown" when neither is configured).
// Stats are per-node and in-memory: a restart clears them, and in a
// cluster each node reports only the traffic it dispatched.

type usageEdgesIn struct {
	Window    string `query:"window" default:"24h"`
	Namespace string `query:"namespace" doc:"Only edges to this namespace."`
	Version   string `query:"version" doc:"Only edges to this version."`
}

type usageEdgeRow struct {
	Caller         string `json:"caller"`
	Namespace      string `json:"namespace"`
	Version        string `json:"version"`
	Method         string `json:"method"`
	Count          uint64 `json:"count"`
	OkCount        uint64 `json:"okCount"`
	LastSeenUnixMs int64  `json:"lastSeenUnixMs"`
}

// usageServiceRow is the per-(namespace, version) rollup. A registered
// service with totalCount 0 had no traffic in the window on this node.
type usageServiceRow struct {
	Namespace      string `json:"namespace"`
	Version        string `json:"version"`
	TotalCount     uint64 `json:"totalCount"`
	CallerCount    int    `json:"callerCount"`
	LastSeenUnixMs int64  `json:"lastSeenUnixMs"`
}

type usageEdgesOut struct {
	Body struct {
		Window   string            `json:"window"`
		Edges    []usageEdgeRow    `json:"edges"`
		Services []usageServiceRow `json:"services"`
	}
}

func registerUsageEdges(api huma.API, g *Gateway, cp *controlPlane) {
	huma.Register(api, huma.Operation{
		OperationID: "usageEdges",
		Method:      http.MethodGet,
		Path:        "/admin/services/usage",
		Summary:     "Active dependencies by method: one edge per (caller, namespace, version, method) with traffic in the window, plus a per-(namespace, version) rollup that includes registered services with no traffic. Per-node, in-memory; caller is 'unknown' unless a caller-id source is configured.",
	}, func(ctx context.Context, in *usageEdgesIn) (*usageEdgesOut, error) {
		window, err := parseStatsWindow(in.Window)
		if err != nil {
			return nil, err
		}
		out := &usageEdgesOut{}
		out.Body.Window = in.Window
		out.Body.Edges = []usageEdgeRow{}
		out.Body.Services = []usageServiceRow{}

		match := func(ns, ver string) bool {
			return (in.Namespace == "" || in.Namespace == ns) &&
				(in.Version == "" || in.Version == ver)
		}
		type agg struct {
			row     usageServiceRow
			callers map[string]struct{}
		}
		svcs := map[serviceKey]*agg{}
		ensure := func(ns, ver string) *agg {
			k := serviceKey{ns, ver}
			a := svcs[k]
			if a == nil {
				a = &agg{row: usageServiceRow{Namespace: ns, Version: ver}, callers: map[string]struct{}{}}
				svcs[k] = a
			}
			return a
		}
		reg, err := cp.ListServices(ctx, &cpv1.ListServicesRequest{})
		if err != nil {
			return nil, err
		}
		for _, s := range reg.GetServices() {
			if match(s.GetNamespace(), s.GetVersion()) {
				ensure(s.GetNamespace(), s.GetVersion())
			}
		}
		for _, r := range g.snapshot(window, nowFunc()) {
			if !match(r.Namespace, r.Version) {
				continue
			}
			out.Body.Edges = append(out.Body.Edges, usageEdgeRow{
				Caller: r.Caller, Namespace: r.Namespace, Version: r.Version,
				Method: r.Method, Count: r.Count, OkCount: r.OkCount,
				LastSeenUnixMs: r.LastSeenUnixMs,
			})
			a := ensure(r.Namespace, r.Version)
			a.row.TotalCount += r.Count
			a.callers[r.Caller] = struct{}{}
			if r.LastSeenUnixMs > a.row.LastSeenUnixMs {
				a.row.LastSeenUnixMs = r.LastSeenUnixMs
			}
		}
		for _, a := range svcs {
			a.row.CallerCount = len(a.callers)
			out.Body.Services = append(out.Body.Services, a.row)
		}
		sort.Slice(out.Body.Edges, func(i, j int) bool {
			a, b := out.Body.Edges[i], out.Body.Edges[j]
			if a.Count != b.Count {
				return a.Count > b.Count
			}
			if a.Namespace != b.Namespace {
				return a.Namespace < b.Namespace
			}
			if a.Version != b.Version {
				return a.Version < b.Version
			}
			if a.Method != b.Method {
				return a.Method < b.Method
			}
			return a.Caller < b.Caller
		})
		sort.Slice(out.Body.Services, func(i, j int) bool {
			a, b := out.Body.Services[i], out.Body.Services[j]
			if a.Namespace != b.Namespace {
				return a.Namespace < b.Namespace
			}
			return a.Version < b.Version
		})
		return out, nil
	})
}
