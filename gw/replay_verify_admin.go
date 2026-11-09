package gateway

import (
	"context"
	"log"
	"net/http"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/danielgtaylor/huma/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// replayMetrics counts replay verifications on blue. Like
// shadowMetrics it lives on the default Prometheus registry only.
type replayMetrics struct {
	verify *prometheus.CounterVec
}

func newReplayMetrics(pm *prometheusMetrics) *replayMetrics {
	m := &replayMetrics{verify: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: pm.prefix + "_replay_verify_total",
		Help: "Replay verifications of recorded requests (WithReplayVerify), by outcome: match, diff, unverifiable (green would make an upstream call blue did not), dropped (queue full, or the request was over MaxBytes or could not be recorded), error (green could not be asked or could not run it).",
	}, []string{"outcome"})}
	pm.registry.MustRegister(m.verify)
	return m
}

func (g *Gateway) recordReplayVerify(outcome string) {
	if g.replayMetrics == nil {
		return
	}
	g.replayMetrics.verify.WithLabelValues(outcome).Inc()
}

// registerReplayRoutes adds the replay routes to the admin router:
// the verify endpoint (plain HTTP, not self-ingested into GraphQL; it
// is machine-to-machine) and the recent-diffs read (huma).
func registerReplayRoutes(mux *http.ServeMux, api huma.API, g *Gateway) {
	if g.cfg.replayEndpoint {
		// Not in the huma table: systemKeyRoutes gives it the admin
		// role or a system key.
		mux.HandleFunc("POST /admin/replay/verify", g.serveReplayVerify)
	}
	huma.Register(api, huma.Operation{
		OperationID: "listReplayDiffs",
		Method:      http.MethodGet,
		Path:        "/admin/replay/diffs",
		Summary:     "Recent replay verifications that did not match (WithReplayVerify), newest last, with outcome counts. Diffs carry client and upstream payload values.",
	}, func(ctx context.Context, _ *struct{}) (*replayDiffsOut, error) {
		out := &replayDiffsOut{}
		out.Body.Items = []replayDiffItem{}
		rr := g.replayRec
		if rr == nil {
			return out, nil
		}
		out.Body.Enabled = true
		out.Body.Target = rr.opts.Target
		out.Body.Counts = replayCounts{
			Match:        rr.countOf(ReplayOutcomeMatch),
			Diff:         rr.countOf(ReplayOutcomeDiff),
			Unverifiable: rr.countOf(ReplayOutcomeUnverifiable),
			Dropped:      rr.countOf(ReplayOutcomeDropped),
			Error:        rr.countOf(ReplayOutcomeError),
		}
		for _, r := range rr.recentResults() {
			item := replayDiffItem{
				ID:            r.result.ID,
				AtUnixMs:      r.at.UnixMilli(),
				OperationName: r.opName,
				Outcome:       r.result.Outcome,
				Error:         r.result.Error,
				Truncated:     r.result.Truncated,
				Legs:          []replayLegSummary{},
				Diffs:         []replayDiffEntry{},
			}
			for _, l := range r.result.Legs {
				item.Legs = append(item.Legs, replayLegSummary{Kind: l.Kind, Namespace: l.Namespace, Version: l.Version, Op: l.Op, Outcome: l.Outcome})
			}
			for _, d := range r.result.Diffs {
				item.Diffs = append(item.Diffs, replayDiffEntry(d))
			}
			out.Body.Items = append(out.Body.Items, item)
		}
		return out, nil
	})
}

type replayCounts struct {
	Match        int64 `json:"match"`
	Diff         int64 `json:"diff"`
	Unverifiable int64 `json:"unverifiable"`
	Dropped      int64 `json:"dropped"`
	Error        int64 `json:"error"`
}

type replayLegSummary struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Op        string `json:"op"`
	Outcome   string `json:"outcome"`
}

type replayDiffEntry struct {
	Path  string `json:"path"`
	Blue  string `json:"blue"`
	Green string `json:"green"`
}

type replayDiffItem struct {
	ID            string             `json:"id"`
	AtUnixMs      int64              `json:"atUnixMs"`
	OperationName string             `json:"operationName"`
	Outcome       string             `json:"outcome"`
	Error         string             `json:"error"`
	Truncated     bool               `json:"truncated"`
	Legs          []replayLegSummary `json:"legs"`
	Diffs         []replayDiffEntry  `json:"diffs"`
}

type replayDiffsOut struct {
	Body struct {
		Enabled bool             `json:"enabled"`
		Target  string           `json:"target"`
		Counts  replayCounts     `json:"counts"`
		Items   []replayDiffItem `json:"items"`
	}
}

// singleOperationName is the name of the document's only operation,
// or "" (anonymous, several, or unparsable).
func singleOperationName(query string) string {
	doc, err := parser.Parse(parser.ParseParams{Source: query})
	if err != nil {
		return ""
	}
	name := ""
	n := 0
	for _, d := range doc.Definitions {
		if op, ok := d.(*ast.OperationDefinition); ok {
			n++
			if op.Name != nil {
				name = op.Name.Value
			}
		}
	}
	if n != 1 {
		return ""
	}
	return name
}

func replayLog(format string, args ...any) {
	log.Printf("gateway: "+format, args...)
}
