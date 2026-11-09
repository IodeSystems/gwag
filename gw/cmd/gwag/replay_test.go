package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

const replaySpec = `{
  "openapi": "3.0.0",
  "info": {"title": "items", "version": "1"},
  "paths": {"/items": {"get": {"operationId": "listItems",
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"type": "string"}}}}}}}}}
}`

func TestReplayVerifyOptions(t *testing.T) {
	if _, err := replayVerifyOptions("", "f", 1, false); err == nil {
		t.Fatal("token file without target: accepted")
	}
	if _, err := replayVerifyOptions("green:8080", "", 1, false); err == nil {
		t.Fatal("target without scheme: accepted")
	}
	for _, r := range []float64{0, -1, 1.5} {
		if _, err := replayVerifyOptions("http://green", "", r, false); err == nil {
			t.Fatalf("rate %v: accepted", r)
		}
	}
	if o, err := replayVerifyOptions("", "", 1, false); err != nil || len(o) != 0 {
		t.Fatalf("off: %v %d", err, len(o))
	}
}

// Blue starts before the token file exists: posts fail (outcome
// error). Once the file holds green's token, the next post matches.
func TestReplayVerifyFlagsTwoGateways(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["a","b"]`))
	}))
	t.Cleanup(upstream.Close)

	newGW := func(opts []gateway.Option) (*gateway.Gateway, *httptest.Server) {
		g := gateway.New(append([]gateway.Option{gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("0123456789abcdef"))}, opts...)...)
		t.Cleanup(g.Close)
		if err := g.AddOpenAPIBytes([]byte(replaySpec), gateway.To(upstream.URL), gateway.As("items")); err != nil {
			t.Fatal(err)
		}
		adminMux, _, err := g.AdminHumaRouter()
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.Handle("/api/graphql", g.Handler())
		mux.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return g, srv
	}

	greenOpts, err := replayVerifyOptions("", "", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	green, greenSrv := newGW(greenOpts)
	tokFile := filepath.Join(t.TempDir(), "green.key")
	blueOpts, err := replayVerifyOptions(greenSrv.URL+"/api/admin/replay/verify", tokFile, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	blue, blueSrv := newGW(blueOpts)

	query := func() {
		t.Helper()
		resp, err := http.Post(blueSrv.URL+"/api/graphql", "application/json", strings.NewReader(`{"query":"query L { items { listItems } }"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("query: %s", resp.Status)
		}
	}
	counts := func() (match, errs int64) {
		req, _ := http.NewRequest(http.MethodGet, blueSrv.URL+"/api/admin/replay/diffs", nil)
		req.Header.Set("Authorization", "Bearer "+blue.AdminTokenHex())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Counts struct{ Match, Error int64 } `json:"counts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Counts.Match, out.Counts.Error
	}

	query()
	waitFor(t, "error outcome without a token file", func() bool { _, e := counts(); return e == 1 })
	if err := os.WriteFile(tokFile, []byte(green.AdminTokenHex()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	query()
	waitFor(t, "match outcome with the token file", func() bool { m, _ := counts(); return m == 1 })
}
