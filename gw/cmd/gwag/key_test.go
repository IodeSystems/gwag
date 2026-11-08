package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestKeyCmd(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("tok")))
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
		code := runKey(append(args, "--endpoint", srv.URL, "--bearer", "tok"), &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errs := run("create", "--ns", "orders", "--ns", "billing", "--ttl", "30d", "--note", "prod")
	key := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(key, "gwk_") || strings.Contains(key, "\n") || !strings.Contains(errs, "shown once") {
		t.Fatalf("create: code=%d out=%q err=%q", code, out, errs)
	}
	id := strings.SplitN(key, "_", 3)[1]
	code, out, _ = run("list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "billing,orders") || !strings.Contains(out, "valid") || strings.Contains(out, key) {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	if code, out, errs := run("revoke", id); code != 0 || !strings.Contains(out, "revoked "+id) {
		t.Fatalf("revoke: code=%d out=%q err=%q", code, out, errs)
	}
	if code, _, errs := run("revoke", id); code != 1 || !strings.Contains(errs, "404") {
		t.Fatalf("revoke again: code=%d err=%q", code, errs)
	}
	if code, out, _ := run("list"); code != 0 || !strings.Contains(out, "no app keys") {
		t.Fatalf("list after revoke: %q", out)
	}
	if code, _, _ := run("create", "--ttl", "30d"); code != 2 {
		t.Fatalf("create without --ns: %d", code)
	}
	var out2, errb bytes.Buffer
	if code := runKey([]string{"list", "--endpoint", srv.URL, "--bearer", g.DevTokenHex()}, &out2, &errb); code != 1 || !strings.Contains(errb.String(), "403") {
		t.Fatalf("list with the dev token: %d %q", code, errb.String())
	}
}
