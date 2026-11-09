package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestParseSwitchoverRule(t *testing.T) {
	r, err := parseSwitchoverRule("op=CreateOrder,header=X-Canary,value=1,percent=100")
	if err != nil || r != (switchoverRule{OperationName: "CreateOrder", Header: "X-Canary", HeaderValue: "1", Percent: 100}) {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := parseSwitchoverRule("op=A,exclude"); err != nil || !r.Exclude || r.OperationName != "A" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"percent=0", "percent=101", "colour=red", "exclude=yes"} {
		if _, err := parseSwitchoverRule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClusterSwitchoverCmd(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("tok")),
		gateway.WithSwitchover(gateway.SwitchoverOptions{Target: "http://green.invalid/api/graphql"}))
	t.Cleanup(g.Close)
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runSwitchover(append(args, "--gateway", srv.URL, "--bearer", "tok"), &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, out, errs := run(); code != 0 || !strings.Contains(out, "source=static active=false") || !strings.Contains(out, "percent=0") {
		t.Fatalf("show: %d %q %q", code, out, errs)
	}
	code, out, errs := run("--percent", "5", "--rule", "op=CreateOrder,exclude", "--rule", "header=X-Canary,percent=100")
	if code != 0 || !strings.Contains(out, "source=runtime active=true target=http://green.invalid/api/graphql percent=5") ||
		!strings.Contains(out, "rule 1: op=CreateOrder,exclude") || !strings.Contains(out, "rule 2: header=X-Canary,percent=100") {
		t.Fatalf("set: %d %q %q", code, out, errs)
	}
	if code, out, _ := run("--clear"); code != 0 || !strings.Contains(out, "source=static") {
		t.Fatalf("clear: %d %q", code, out)
	}
	if code, _, _ := run("--rule", "op=A"); code != 2 {
		t.Fatalf("--rule without --percent: %d", code)
	}
	if code, _, _ := run("--clear", "--percent", "5"); code != 2 {
		t.Fatalf("--clear with --percent: %d", code)
	}
	var out2, errb bytes.Buffer
	if code := runSwitchover([]string{"--percent", "50", "--gateway", srv.URL, "--bearer", g.DevTokenHex()}, &out2, &errb); code != 1 || !strings.Contains(errb.String(), "403") {
		t.Fatalf("set with the dev token: %d %q", code, errb.String())
	}
}
