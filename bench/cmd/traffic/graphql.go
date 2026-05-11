package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/iodesystems/gwag/bench/cmd/traffic/runner"
)

// runGraphQL parses graphql-adapter flags, builds one Target per
// --target URL with a JSON-POST Fire closure, and hands off to
// runner.Run.
func runGraphQL(args []string) error {
	fs := flag.NewFlagSet("graphql", flag.ExitOnError)
	rps := fs.Int("rps", 100, "requests per second per target")
	duration := fs.Duration("duration", 30*time.Second, "test duration")
	concurrency := fs.Int("concurrency", 0, "max concurrent in-flight per target (extras are dropped); 0 = auto = max(64, rps/20)")
	shards := fs.Int("shards", 0, "driver goroutines per target; 0 = auto = ceil(rps/1500). Sub-millisecond Go tickers cap at ~3k Hz per goroutine, so anything above ~3k RPS must be sharded.")
	timeout := fs.Duration("timeout", 5*time.Second, "per-request HTTP timeout")
	serverSide := fs.Bool("server-metrics", true, "snapshot gateway /api/metrics before+after for the per-backend table")
	jsonOut := fs.String("json", "", "write the gateway-pass summary (target_rps, achieved RPS, p50/p95/p99, gateway dispatch + ingress) to PATH as JSON. Direct-pass results are not exported. PATH '-' writes to stdout.")
	query := fs.String("query", `{ greeter { hello(name: "world") { greeting } } }`, "GraphQL query string")
	directQuery := fs.String("direct-query", "", "GraphQL query string for the --direct pass. Defaults to --query — but the upstream's schema is usually unprefixed, so override (e.g. '{ hello(name:\"world\") { greeting } }') when the gateway adds a namespace.")
	var targetsRaw runner.StringFlag
	fs.Var(&targetsRaw, "target", "GraphQL endpoint URL (repeat or comma-separate for multiple)")
	var directTargetsRaw runner.StringFlag
	fs.Var(&directTargetsRaw, "direct", "upstream GraphQL endpoint URL (e.g. http://localhost:50054/graphql) to dial directly, bypassing the gateway. When set, runs a second pass and prints a side-by-side compare. Repeat or comma-separate for multiple direct targets.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	targetURLs := runner.SplitCSV(targetsRaw)
	if len(targetURLs) == 0 {
		return errors.New("at least one --target is required")
	}

	body, err := json.Marshal(map[string]any{"query": *query})
	if err != nil {
		return fmt.Errorf("marshal query: %w", err)
	}

	targets := make([]runner.Target, 0, len(targetURLs))
	for _, u := range targetURLs {
		fire := makeGraphQLFire(*timeout, *concurrency, u, body)
		targets = append(targets, runner.Target{
			Label:      u,
			MetricsURL: runner.MetricsURLFromGateway(u),
			Fire:       fire,
		})
	}

	directURLs := runner.SplitCSV(directTargetsRaw)
	directBody := body
	if len(directURLs) > 0 && *directQuery != "" {
		directBody, err = json.Marshal(map[string]any{"query": *directQuery})
		if err != nil {
			return fmt.Errorf("marshal direct-query: %w", err)
		}
	}
	directTargets := make([]runner.Target, 0, len(directURLs))
	for _, u := range directURLs {
		fire := makeGraphQLFire(*timeout, *concurrency, u, directBody)
		directTargets = append(directTargets, runner.Target{
			Label: "direct " + u,
			// MetricsURL empty: gateway not in path on this pass.
			Fire: fire,
		})
	}

	opts := runner.Options{
		RPS:           *rps,
		Duration:      *duration,
		Concurrency:   *concurrency,
		Shards:        *shards,
		ServerMetrics: *serverSide,
	}

	fmt.Fprintf(os.Stdout, "running %d req/s for %s against %d target(s)\n", *rps, duration.String(), len(targetURLs))
	gwRes, err := runner.Run(opts, ternaryStr(len(directTargets) > 0, "gateway", ""), targets)
	if err != nil {
		return err
	}
	runner.PrintPass(opts, gwRes)
	if err := writeJSONIfRequested(*jsonOut, opts, gwRes); err != nil {
		return err
	}

	if len(directTargets) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stdout, "\nrunning direct pass: %d req/s for %s against %d direct target(s); bypassing gateway\n", *rps, duration.String(), len(directTargets))
	directOpts := opts
	directOpts.ServerMetrics = false
	dRes, err := runner.Run(directOpts, "direct", directTargets)
	if err != nil {
		return err
	}
	runner.PrintPass(directOpts, dRes)
	runner.PrintCompare(gwRes, dRes)
	return nil
}

func makeGraphQLFire(timeout time.Duration, concurrency int, target string, body []byte) func(context.Context, *runner.Stats) {
	// Default http.Transport sets MaxIdleConnsPerHost=2 — at any real
	// load, requests beyond that pair burn fresh TCP connections, pile
	// up in TIME_WAIT, and after a few seconds surface as "connect:
	// cannot assign requested address". Size the pool to match
	// concurrency so keep-alive actually works.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = concurrency * 4
	tr.MaxIdleConnsPerHost = concurrency * 4
	tr.IdleConnTimeout = 90 * time.Second
	client := &http.Client{Timeout: timeout, Transport: tr}
	return func(ctx context.Context, s *runner.Stats) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
		if err != nil {
			s.RecordErr(runner.ErrTransport, "build request: "+err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			// tcancel() at end-of-duration fails any in-flight request
			// with this error; indistinguishable from real timeouts at
			// the syscall level. Drop the sample.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.RecordErr(runner.ErrTransport, err.Error())
			return
		}
		defer resp.Body.Close()

		statusLabel := fmt.Sprintf("%d", resp.StatusCode)
		s.RecordCode(statusLabel)

		respBody, _ := io.ReadAll(resp.Body)
		excerpt := runner.Truncate(string(respBody), 200)
		if resp.StatusCode != 200 {
			s.RecordErr(runner.ErrHTTP, fmt.Sprintf("status=%d body=%s", resp.StatusCode, excerpt))
			s.RecordBody(statusLabel, excerpt)
			return
		}
		var env struct {
			Errors []struct {
				Message    string         `json:"message"`
				Extensions map[string]any `json:"extensions"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(respBody, &env); err == nil && len(env.Errors) > 0 {
			first := env.Errors[0]
			code := ""
			if c, ok := first.Extensions["code"].(string); ok {
				code = " code=" + c
			}
			s.RecordErr(runner.ErrEnvelope, fmt.Sprintf("%s%s", first.Message, code))
			s.RecordBody("200 (graphql errors)", excerpt)
			return
		}
		s.RecordBody(statusLabel, excerpt)
		s.RecordOK(elapsed)
	}
}
