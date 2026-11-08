package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/events/schemaregistry/srtest"
	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestSchemaImportCmd(t *testing.T) {
	src := srtest.New()
	defer src.Close()
	id, _ := src.Add("corp-value", sr.Schema{Schema: `syntax = "proto3"; package corp; message Order { string id = 1; }`, SchemaType: "PROTOBUF"})
	id2, _ := src.Add("corp-value", sr.Schema{Schema: `syntax = "proto3"; package corp; message Order { string id = 1; int64 total = 2; }`, SchemaType: "PROTOBUF"})

	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithSchemaRegistry(gateway.SchemaRegistryOptions{}))
	defer g.Close()
	mux := http.NewServeMux()
	f := &schemaRegistryFlags{on: new(bool)}
	*f.on = true
	f.mount(mux, g)
	dst := httptest.NewServer(mux)
	defer dst.Close()
	to := dst.URL + schemaRegistryPrefix

	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := schemaImportCmd(args, &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := run("--from", src.URL, "--to", to, "--bearer", g.AdminTokenHex()); code != 2 || !strings.Contains(out, "--into") {
		t.Fatalf("no --into: %d %s", code, out)
	}
	if code, out := run("--from", src.URL, "--to", to, "--into", ".", "--bearer", g.AdminTokenHex()); code != 2 || !strings.Contains(out, "default context") {
		t.Fatalf("--into .: %d %s", code, out)
	}
	if code, out := run("--from", src.URL, "--to", to, "--into", ".corp"); code != 1 || !strings.Contains(out, "40101") {
		t.Fatalf("no token: %d %s", code, out)
	}
	code, out := run("--from", src.URL, "--to", to, "--into", ".corp", "--bearer", g.AdminTokenHex())
	if code != 0 || !strings.Contains(out, "2 schemas added, 2 versions added") || !strings.Contains(out, "/contexts/.corp") {
		t.Fatalf("import: %d %s", code, out)
	}
	code, out = run("--from", src.URL, "--to", to, "--into", "corp", "--bearer", g.AdminTokenHex())
	if code != 0 || !strings.Contains(out, "0 schemas added, 0 versions added, 2 unchanged") {
		t.Fatalf("re-import: %d %s", code, out)
	}
	c, err := sr.NewClient(sr.Config{URL: to + "/contexts/.corp"})
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]int{"1": id, "2": id2} {
		got, err := c.SubjectVersion(context.Background(), "corp-value", v, "")
		if err != nil || got.ID != want {
			t.Errorf("version %s: %+v %v, want id %d", v, got, err, want)
		}
	}
}
