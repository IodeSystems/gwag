package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MakeGraphQLFire returns a per-request Fire closure for runner.Run
// that POSTs a fixed JSON body to one GraphQL endpoint. The HTTP
// client + Transport are built once per call and owned by the
// closure, so the keep-alive pool persists for the lifetime of the
// closure — across reps + steps when the closure is reused. Size
// the pool for the largest concurrency the closure will ever see;
// undersizing forces TCP churn into TIME_WAIT and surfaces as
// "connect: cannot assign requested address" at sustained load.
//
// Moved from the traffic main package so non-traffic drivers (the
// perf sweep, in particular) can build the same closure in-process
// without exec'ing the traffic binary per rep.
func MakeGraphQLFire(timeout time.Duration, concurrency int, target string, body []byte) func(context.Context, *Stats) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = concurrency * 4
	tr.MaxIdleConnsPerHost = concurrency * 4
	tr.IdleConnTimeout = 90 * time.Second
	client := &http.Client{Timeout: timeout, Transport: tr}
	return func(ctx context.Context, s *Stats) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
		if err != nil {
			s.RecordErr(ErrTransport, "build request: "+err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.RecordErr(ErrTransport, err.Error())
			return
		}
		defer resp.Body.Close()

		statusLabel := fmt.Sprintf("%d", resp.StatusCode)
		s.RecordCode(statusLabel)

		respBody, _ := io.ReadAll(resp.Body)
		excerpt := Truncate(string(respBody), 200)
		if resp.StatusCode != 200 {
			s.RecordErr(ErrHTTP, fmt.Sprintf("status=%d body=%s", resp.StatusCode, excerpt))
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
			s.RecordErr(ErrEnvelope, fmt.Sprintf("%s%s", first.Message, code))
			s.RecordBody("200 (graphql errors)", excerpt)
			return
		}
		s.RecordBody(statusLabel, excerpt)
		s.RecordOK(elapsed)
	}
}
