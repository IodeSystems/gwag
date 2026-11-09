package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func cutoverServer(t *testing.T) string {
	t.Helper()
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("tok")),
		gateway.WithSchemaRegistry(gateway.SchemaRegistryOptions{Writes: true}))
	t.Cleanup(g.Close)
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClusterStateCmd(t *testing.T) {
	blue, green := cutoverServer(t), cutoverServer(t)
	run := func(url, bearer string, stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := clusterStateCmd(append(args, "--gateway", url, "--bearer", bearer), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	key := func(url string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runKey(append(args, "--endpoint", url, "--bearer", "tok"), &out, &errb)
		return code, out.String(), errb.String()
	}

	code, sysOut, errs := key(blue, "create", "--system", "--ttl", "1d", "--note", "cutover")
	sys := strings.TrimSpace(sysOut)
	if code != 0 || !strings.HasPrefix(sys, "gws_") || !strings.Contains(errs, "system key id") {
		t.Fatalf("key create --system: %d %q %q", code, sysOut, errs)
	}
	if code, _, _ := key(blue, "create", "--system", "--ns", "x", "--ttl", "1d"); code != 2 {
		t.Fatalf("key create --system --ns: %d, want 2", code)
	}
	code, appOut, _ := key(blue, "create", "--ns", "orders", "--ttl", "1d")
	if code != 0 {
		t.Fatalf("key create: %d", code)
	}
	if code, out, _ := key(blue, "list"); code != 0 || !strings.Contains(out, "system") || !strings.Contains(out, " app ") || !strings.Contains(out, "orders") {
		t.Fatalf("key list: %d %q", code, out)
	}

	if code, _, errs := run(blue, sys, "", "export"); code != 1 || !strings.Contains(errs, "not frozen") {
		t.Fatalf("export unfrozen: %d %q", code, errs)
	}
	if code, out, errs := run(blue, sys, "", "freeze"); code != 1 || !strings.Contains(errs, "403") {
		t.Fatalf("freeze with the system key: %d %q %q", code, out, errs)
	}
	if code, out, errs := run(blue, "tok", "", "freeze", "--ttl", "10m"); code != 0 || !strings.Contains(out, "frozen until") {
		t.Fatalf("freeze: %d %q %q", code, out, errs)
	}
	if code, _, errs := key(blue, "create", "--ns", "x", "--ttl", "1d"); code != 1 || !strings.Contains(errs, "409") || !strings.Contains(errs, "cluster frozen for cutover") {
		t.Fatalf("key create while frozen: %d %q", code, errs)
	}
	code, doc, errs := run(blue, sys, "", "export")
	if code != 0 || !strings.Contains(doc, `"v": 1`) || strings.Contains(doc, "$schema") {
		t.Fatalf("export: %d %q %q", code, doc, errs)
	}
	file := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := run(green, "tok", "", "import", "--dry-run", file); code != 0 || !strings.Contains(out, "dry run: stable 0 added") || !strings.Contains(out, "keys 2 added") {
		t.Fatalf("import --dry-run: %d %q %q", code, out, errs)
	}
	if code, out, errs := run(green, "tok", "", "import", file); code != 0 || !strings.Contains(out, "imported:") || !strings.Contains(out, "keys 2 added") {
		t.Fatalf("import: %d %q %q", code, out, errs)
	}
	// Again from stdin, with blue's system key: now valid on green.
	if code, out, errs := run(green, sys, doc, "import", "-"); code != 0 || !strings.Contains(out, "keys 0 added, 2 unchanged") {
		t.Fatalf("import again: %d %q %q", code, out, errs)
	}
	if code, out, _ := key(green, "list"); code != 0 || !strings.Contains(out, strings.SplitN(strings.TrimSpace(appOut), "_", 3)[1]) {
		t.Fatalf("green key list: %q", out)
	}
	if code, out, errs := run(blue, "tok", "", "unfreeze"); code != 0 || out != "unfrozen\n" {
		t.Fatalf("unfreeze: %d %q %q", code, out, errs)
	}
	if code, out, _ := run(blue, "tok", "", "unfreeze"); code != 0 || out != "was not frozen\n" {
		t.Fatalf("unfreeze again: %d %q", code, out)
	}
}
