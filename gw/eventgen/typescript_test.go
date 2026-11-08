package eventgen

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// needTS skips when the TS example's pinned tools are not installed.
func needTS(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dir := filepath.Join(root, "examples/events/ts")
	if _, err := os.Stat(filepath.Join(dir, "node_modules/.bin/protoc-gen-es")); err != nil {
		t.Skip("run `pnpm install` in examples/events/ts")
	}
	return dir
}

// The committed TS example is what `gwag gen --lang ts` writes today.
func TestTSExampleUpToDate(t *testing.T) {
	dir := needTS(t)
	root := repoRoot(t)
	out := t.TempDir()
	written, err := Generate(context.Background(), Config{
		Lang:        "ts",
		Out:         out,
		ImportPaths: []string{filepath.Join(root, "examples/events/proto")},
		Files:       []string{"orders/v1/orders.proto"},
		Opts:        []string{"target=ts", "import_extension=ts", "erasable_syntax=true"},
		ModuleDir:   dir,
		GwagVersion: "(devel)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, " ") != "orders/v1/orders_events.ts orders/v1/orders_pb.ts" {
		t.Fatalf("written = %v", written)
	}
	for _, w := range written {
		got, _ := os.ReadFile(filepath.Join(out, w))
		want, err := os.ReadFile(filepath.Join(dir, "src/gen", w))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from examples/events/ts/src/gen; run examples/events/check.sh to regenerate", w)
		}
	}
}

func TestTSLanguageAlias(t *testing.T) {
	if languages["typescript"] == nil || languages["ts"] == nil {
		t.Fatal("ts and typescript are both registered")
	}
}

func TestTSMissingToolNamesInstallCommand(t *testing.T) {
	dir := writeProtos(t, map[string]string{"shop.proto": header})
	_, err := Generate(context.Background(), Config{
		Lang: "ts", Out: t.TempDir(), ImportPaths: []string{dir}, Files: []string{"shop.proto"}, ModuleDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "pnpm add --save-dev --save-exact @bufbuild/protoc-gen-es@") {
		t.Fatalf("err = %v", err)
	}
}

func TestTSParams(t *testing.T) {
	p := parseTSParams("target=ts,events_import=@acme/events,import_extension=js")
	if p.eventsImport != "@acme/events" || p.importExt != ".js" || strings.Join(p.plugin, ",") != "target=ts,import_extension=js" {
		t.Fatalf("%+v", p)
	}
	if d := parseTSParams(""); d.eventsImport != defaultEventsImport || d.importExt != "" {
		t.Fatalf("%+v", d)
	}
}

func TestTSEmitterErrors(t *testing.T) {
	cases := map[string]string{
		"collides with imported message": `
message FooSubject {}
message Foo {
  option (gwag.ps.v1.binding) = { name: "Foo", pattern: "a" };
  oneof v { FooSubject a = 1; }
}`,
		"union member named unknown": `
message unknown { option (gwag.ps.v1.binding) = { name: "U", pattern: "u" }; }
message Known { option (gwag.ps.v1.binding) = { name: "U", pattern: "u" }; }`,
		"oneof case named unknown": `
message M { option (gwag.ps.v1.binding) = { name: "M", pattern: "m" }; oneof v { string unknown = 1; } }`,
		"segments collapse": `
message M { option (gwag.ps.v1.binding) = { name: "M", pattern: "m.{order_id}.{orderId}" }; }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeProtos(t, map[string]string{"shop.proto": header + body})
			files, _, err := compile(context.Background(), []string{dir}, []string{"shop.proto"})
			if err != nil {
				t.Fatal(err)
			}
			chans, err := Channels(files)
			if err != nil {
				t.Fatal(err)
			}
			_, err = emitTS(&Input{Files: files, Channels: chans, Header: Header{GwagVersion: "(devel)"}, Request: nil})
			if err == nil {
				t.Fatal("no error")
			}
		})
	}
}

// Generated code for the edge shapes typechecks under the example's
// tsconfig: a channel with no segments, enum / bytes / int64 /
// well-known-type oneof cases, a nested union member, a message in a
// sibling directory.
func TestTSOutputTypechecks(t *testing.T) {
	dir := needTS(t)
	tmp := filepath.Join(dir, "edge-test-tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	src := writeProtos(t, map[string]string{
		"common/v1/common.proto": `syntax = "proto3";
package common.v1;
message Tag { string v = 1; }
enum Level { LEVEL_UNSPECIFIED = 0; LEVEL_HIGH = 1; }`,
		"edge/v1/edge.proto": `syntax = "proto3";
package edge.v1;
import "gw/proto/ps/v1/options.proto";
import "google/protobuf/timestamp.proto";
import "common/v1/common.proto";
message Ping {
  option (gwag.ps.v1.binding) = { name: "Ping", pattern: "edge.ping" };
}
message Mixed {
  option (gwag.ps.v1.binding) = { name: "mixed", pattern: "edge.{order_id}.*.>" };
  oneof v {
    common.v1.Level level = 1;
    bytes raw = 2;
    google.protobuf.Timestamp at = 3;
    double ratio = 4;
    int64 big = 5;
    common.v1.Tag tag = 6;
    string snake_case_name = 7;
  }
}
message Outer {
  option (gwag.ps.v1.binding) = { name: "Nest", pattern: "edge.nest" };
  message Inner {
    option (gwag.ps.v1.binding) = { name: "Nest", pattern: "edge.nest" };
    string x = 1;
  }
}
`})
	_, err := Generate(context.Background(), Config{
		Lang: "ts", Out: tmp, ImportPaths: []string{src},
		Files:     []string{"edge/v1/edge.proto", "common/v1/common.proto"},
		Opts:      []string{"target=ts", "import_extension=ts", "erasable_syntax=true"},
		ModuleDir: dir, GwagVersion: "(devel)",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := `{"extends":"../tsconfig.json","include":["."]}`
	if err := os.WriteFile(filepath.Join(tmp, "tsconfig.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "node_modules/.bin/tsc"), "--noEmit", "-p", tmp)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tsc: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "edge/v1/edge_events.ts"))
	ev := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		`export type PingSubject = Record<string, never>;`,
		`export type MixedSubject = { orderId: string; p1: string; rest: string };`,
		`level?: (subject: MixedSubject, msg: Level,`,
		`raw?: (subject: MixedSubject, msg: Uint8Array,`,
		`at?: (subject: MixedSubject, msg: Timestamp,`,
		`big?: (subject: MixedSubject, msg: bigint,`,
		`snakeCaseName?: (subject: MixedSubject, msg: string,`,
		`Outer_Inner?: (subject: NestSubject, msg: Outer_Inner,`,
		`from "../../common/v1/common_pb.ts"`,
		`from "@bufbuild/protobuf/wkt"`,
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
}
