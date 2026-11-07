package gateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// HTTP server timeouts used by NewHTTPServer.
//
//   - ReadHeaderTimeout 10s: bounds Slowloris-style header dribbling.
//     Headers of a real request arrive in one round trip.
//   - ReadTimeout 0: a body read deadline would cut slow uploads, and
//     the header deadline already covers the attack.
//   - WriteTimeout 0: SSE streams, server-streaming REST and
//     subscriptions write for as long as the stream lives. A write
//     deadline would cut them. Unary latency is bounded by the
//     per-pool upstream timeout, not by the server.
//   - IdleTimeout 120s: reaps idle keep-alive connections. Longer
//     than common LB idle timeouts (60s), so the LB closes first.
const (
	httpReadHeaderTimeout = 10 * time.Second
	httpIdleTimeout       = 120 * time.Second
)

// DefaultShutdownTimeout is the default budget for each bounded phase
// of a graceful shutdown (stream drain, then in-flight request wait).
const DefaultShutdownTimeout = 30 * time.Second

// NewHTTPServer returns an http.Server with the gateway's timeouts
// (see the constants above for the reasoning). Use it in place of
// http.ListenAndServe.
//
// Stability: experimental
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// ShutdownOptions tunes ServeHTTP's shutdown sequence.
//
// Stability: experimental
type ShutdownOptions struct {
	// Delay is how long to keep serving after /health flips to 503, so
	// a load balancer sees the failing probe and stops sending new
	// requests. A node that closes its listener sooner than one probe
	// interval refuses connections the LB still routes to it. Set it to
	// at least probe interval * unhealthy threshold. 0 = no wait.
	Delay time.Duration
	// Timeout bounds the stream drain and, separately, the wait for
	// in-flight HTTP requests. Zero means DefaultShutdownTimeout.
	Timeout time.Duration
	// Teardown runs last, after HTTP has stopped: gRPC GracefulStop,
	// Cluster.Close and the like. May be nil.
	Teardown func()
}

// ServeHTTP serves srv until it fails or the process gets SIGINT or
// SIGTERM, then shuts down in this order:
//
//  1. g.Drain: /health goes 503, WebSockets are cancelled, then it
//     waits for streams (bounded by Timeout). Skipped when g is nil.
//  2. Sleep Delay, so load balancers see the 503.
//  3. srv.Shutdown: closes listeners, waits for in-flight requests
//     (bounded by Timeout; then connections are closed).
//  4. opts.Teardown.
//
// Hijacked connections (WebSocket) are not tracked by srv.Shutdown;
// step 1 is what ends them. A second signal during shutdown exits the
// process at once. A listen error returns immediately, without drain.
//
// Stability: experimental
func ServeHTTP(srv *http.Server, g *Gateway, opts ShutdownOptions) error {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultShutdownTimeout
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-sigs:
	}
	go func() {
		<-sigs
		log.Printf("second signal: exiting without waiting")
		os.Exit(1)
	}()

	if g != nil {
		log.Printf("draining...")
		ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		if err := g.Drain(ctx); err != nil {
			log.Printf("drain returned: %v (proceeding with shutdown)", err)
		} else {
			log.Printf("drained cleanly")
		}
		cancel()
	}
	if opts.Delay > 0 {
		log.Printf("shutdown delay %s (waiting for load balancers)", opts.Delay)
		time.Sleep(opts.Delay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	err := srv.Shutdown(ctx)
	cancel()
	if err != nil {
		log.Printf("http shutdown: %v (closing remaining connections)", err)
		_ = srv.Close()
	} else {
		log.Printf("http requests finished")
	}
	if opts.Teardown != nil {
		opts.Teardown()
	}
	if e := <-errc; e != nil && !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	return nil
}
