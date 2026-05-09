package gateway

import (
	"net/http"
	"net/http/pprof"
)

// PprofMux returns a *http.ServeMux carrying the standard net/http/pprof
// handlers, or nil when WithPprof was not set. Routes mirror the
// defaults net/http/pprof installs on http.DefaultServeMux:
//
//	/debug/pprof/         pprof.Index (dispatches goroutine, heap,
//	                      allocs, threadcreate, block, mutex profiles)
//	/debug/pprof/cmdline  pprof.Cmdline
//	/debug/pprof/profile  pprof.Profile (CPU)
//	/debug/pprof/symbol   pprof.Symbol
//	/debug/pprof/trace    pprof.Trace
//
// The returned mux owns the path layout — wire it under any prefix and
// behind whatever auth fits the deployment. pprof leaks goroutine and
// heap state, so never expose it publicly.
func (g *Gateway) PprofMux() *http.ServeMux {
	if !g.cfg.pprof {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
