package gateway

import (
	"encoding/json"
	"sync"
	"time"
)

// requestLogLine is the JSON shape `WithRequestLog` emits per
// request. Fields are intentionally minimal — the option exists for
// low-load eyeballing, not centralized observability — but enough
// to answer "what just happened, and was it gateway-side or
// upstream?": ingress identifies the entry path, total_us is the
// wall-clock budget, self_us is total minus the dispatch sum, and
// dispatch_count tells you whether the request fanned out.
type requestLogLine struct {
	TS            string `json:"ts"`
	Ingress       string `json:"ingress"`
	Path          string `json:"path,omitempty"`
	TotalUS       int64  `json:"total_us"`
	SelfUS        int64  `json:"self_us"`
	DispatchCount int    `json:"dispatch_count"`
}

// requestLogMu serialises writes to cfg.requestLog so JSON lines
// don't interleave under concurrent ingress traffic. Cheap to take
// (single-line writes); contention only matters when the option is
// pointed at a slow writer, in which case the back-pressure is the
// signal — drop the option or use a buffered Writer.
var requestLogMu sync.Mutex

// logRequestLine writes one JSON line per request when WithRequestLog
// is configured. Encodes from a struct rather than building bytes by
// hand to keep the shape consistent across ingress paths and survive
// quirks in any of the captured strings (path with quotes, etc.).
//
// Total / dispatchSum are read at the call site (caller already
// computed them from the per-request accumulator); the helper is
// shared across the three ingress paths so a future field addition
// lands once.
func (g *Gateway) logRequestLine(ingress, path string, total, dispatchSum time.Duration, dispatchCount int) {
	w := g.cfg.requestLog
	if w == nil {
		return
	}
	line := requestLogLine{
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Ingress:       ingress,
		Path:          path,
		TotalUS:       total.Microseconds(),
		SelfUS:        (total - dispatchSum).Microseconds(),
		DispatchCount: dispatchCount,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	b = append(b, '\n')
	requestLogMu.Lock()
	_, _ = w.Write(b)
	requestLogMu.Unlock()
}
