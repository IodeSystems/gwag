package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestLimitQuotaCostUsageCmds(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("tok")), gateway.WithUsage(gateway.UsageOptions{}))
	t.Cleanup(g.Close)
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	run := func(cmd string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		args = append(args, "--endpoint", srv.URL, "--bearer", "tok")
		var code int
		if cmd == "usage" {
			code = runUsage(args, &out, &errb)
		} else {
			code = runLimits(cmd, args, &out, &errb)
		}
		return code, out.String(), errb.String()
	}
	if code, out, errs := run("limit", "set", "--service", "vendor:v1", "--op", "search", "--rps", "2.5", "--burst", "3"); code != 0 || !strings.Contains(out, "rate limit vendor:v1 search: 2.5/s burst 3") {
		t.Fatalf("limit set: %d %q %q", code, out, errs)
	}
	if code, out, _ := run("limit", "list"); code != 0 || !strings.Contains(out, "vendor:v1") || !strings.Contains(out, "2.5") || !strings.Contains(out, "admin") {
		t.Fatalf("limit list: %d %q", code, out)
	}
	if code, out, errs := run("quota", "set", "--default", "--service", "vendor", "--limit", "100", "--per", "1d", "--units"); code != 0 || !strings.Contains(out, "quota vendor for default: 100 units per 1d") {
		t.Fatalf("quota set: %d %q %q", code, out, errs)
	}
	if code, _, errs := run("quota", "set", "--caller", "a", "--default", "--limit", "1", "--per", "1m"); code != 2 {
		t.Fatalf("quota set with caller and default: %d %q", code, errs)
	}
	if code, out, _ := run("quota", "list"); code != 0 || !strings.Contains(out, "(default)") || !strings.Contains(out, "units") {
		t.Fatalf("quota list: %d %q", code, out)
	}
	if code, out, errs := run("cost", "set", "--service", "vendor:v1", "--op", "search", "--units", "5"); code != 0 || !strings.Contains(out, "cost vendor:v1 search: 5 units") {
		t.Fatalf("cost set: %d %q %q", code, out, errs)
	}
	if code, out, _ := run("quota", "delete", "--default", "--service", "vendor"); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("quota delete: %d %q", code, out)
	}
	if code, out, _ := run("quota", "delete", "--default", "--service", "vendor"); code != 0 || !strings.Contains(out, "nothing to delete") {
		t.Fatalf("quota delete again: %d %q", code, out)
	}
	if code, _, errs := run("limit", "set", "--service", "vendor:bad", "--rps", "1"); code != 1 || !strings.Contains(errs, "422") {
		t.Fatalf("limit set with a bad version: %d %q", code, errs)
	}
	if code, out, errs := run("usage"); code != 0 || !strings.Contains(out, "(no usage)") {
		t.Fatalf("usage: %d %q %q", code, out, errs)
	}
	if code, out, errs := run("usage", "--csv"); code != 0 || !strings.HasPrefix(out, "day,caller,namespace") {
		t.Fatalf("usage --csv: %d %q %q", code, out, errs)
	}
}
