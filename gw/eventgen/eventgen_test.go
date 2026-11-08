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

// "go-events" is Go without the protoc-gen-go subprocess: only the
// event emitter runs.
func init() {
	Register(&Language{
		Name:    "go-events",
		Plugins: func(context.Context, Env) ([]Plugin, error) { return nil, nil },
		Emitter: EmitterFunc(emitGo),
	})
}

func writeProtos(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const header = `syntax = "proto3";
package shop.v1;
import "gwag/ps/v1/options.proto";
option go_package = "example.com/shop/v1;shopv1";
`

func channelsOf(t *testing.T, body string) ([]*Channel, error) {
	t.Helper()
	dir := writeProtos(t, map[string]string{"shop.proto": header + body})
	files, _, err := compile(context.Background(), []string{dir}, []string{"shop.proto"})
	if err != nil {
		t.Fatal(err)
	}
	return Channels(files)
}

// A registry schema has no binding; ChannelsWith binds it from outside.
func TestChannelsWithBindings(t *testing.T) {
	dir := writeProtos(t, map[string]string{"corp.proto": "syntax = \"proto3\";\npackage corp.v1;\nmessage Order { string id = 1; }\n"})
	files, err := Compile(context.Background(), []string{dir}, []string{"corp.proto"})
	if err != nil {
		t.Fatal(err)
	}
	if chans, err := Channels(files); err != nil || len(chans) != 0 {
		t.Fatalf("Channels = %v, %v", chans, err)
	}
	chans, err := ChannelsWith(files, map[string]Binding{"corp.v1.Order": {Name: "Order", Pattern: "orders"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 || chans[0].Key != "corp.v1.Order" || chans[0].Pattern.String() != "orders" || chans[0].Kind != KindPlain {
		t.Fatalf("ChannelsWith = %+v", chans)
	}
}

func TestChannelKinds(t *testing.T) {
	chans, err := channelsOf(t, `
message Plain {
  option (gwag.ps.v1.binding) = { name: "Plain", pattern: "shop.plain.{id}" };
  string id = 1;
  optional string note = 2;
}
message Choice {
  option (gwag.ps.v1.binding) = { name: "Choice", pattern: "shop.choice" };
  oneof body { string a = 1; int64 b = 2; }
}
message Created {
  option (gwag.ps.v1.binding) = { name: "Lifecycle", pattern: "shop.life.*" };
  message Nested {
    option (gwag.ps.v1.binding) = { name: "Lifecycle", pattern: "shop.life.*" };
  }
}
message Deleted {
  option (gwag.ps.v1.binding) = { name: "Lifecycle", pattern: "shop.life.*" };
}
message Unnamed {
  option (gwag.ps.v1.binding) = { pattern: "shop.unnamed" };
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 3 {
		t.Fatalf("got %d channels, want 3", len(chans))
	}
	if c := chans[0]; c.Key != "shop.v1.Plain" || c.Kind != KindPlain || c.Stream != "EV_shop_v1_Plain" {
		t.Errorf("plain: %+v", c)
	}
	if c := chans[1]; c.Kind != KindOneof || c.Oneof.Name() != "body" {
		t.Errorf("oneof: kind=%v", c.Kind)
	}
	c := chans[2]
	if c.Kind != KindUnion || len(c.Members) != 3 {
		t.Fatalf("union: kind=%v members=%d", c.Kind, len(c.Members))
	}
	var names []string
	for _, m := range c.Members {
		names = append(names, string(m.FullName()))
	}
	if got := strings.Join(names, ","); got != "shop.v1.Created,shop.v1.Created.Nested,shop.v1.Deleted" {
		t.Errorf("members = %s", got)
	}
}

func TestChannelErrors(t *testing.T) {
	for name, body := range map[string]string{
		"pattern mismatch": `
message A { option (gwag.ps.v1.binding) = { name: "X", pattern: "a.b" }; }
message B { option (gwag.ps.v1.binding) = { name: "X", pattern: "a.c" }; }`,
		"bad name":    `message A { option (gwag.ps.v1.binding) = { name: "1x", pattern: "a" }; }`,
		"bad pattern": `message A { option (gwag.ps.v1.binding) = { name: "A", pattern: "a.>.b" }; }`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := channelsOf(t, body); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func genEvents(t *testing.T, files map[string]string, names ...string) (string, error) {
	t.Helper()
	dir := writeProtos(t, files)
	out := t.TempDir()
	_, err := Generate(context.Background(), Config{
		Lang:        "go-events",
		Out:         out,
		ImportPaths: []string{dir},
		Files:       names,
		Opts:        []string{"paths=source_relative"},
		GwagVersion: "v0.0.0-test",
	})
	return out, err
}

func TestGoEmitterErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"collides with message": {"shop.proto": header + `
message Lifecycle {}
message Created { option (gwag.ps.v1.binding) = { name: "Life", pattern: "a" }; }
message LifeSubject {}`},
		"two files one package": {
			"a.proto": header + `message A { option (gwag.ps.v1.binding) = { name: "A", pattern: "a" }; }`,
			"b.proto": header + `message B { option (gwag.ps.v1.binding) = { name: "B", pattern: "b" }; }`,
		},
		"union member named Unknown": {"shop.proto": header + `
message Unknown { option (gwag.ps.v1.binding) = { name: "U", pattern: "u" }; }
message Known { option (gwag.ps.v1.binding) = { name: "U", pattern: "u" }; }`},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			var names []string
			for n := range files {
				names = append(names, n)
			}
			if _, err := genEvents(t, files, names...); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestGoEmitterSkipsFilesWithoutChannels(t *testing.T) {
	out, err := genEvents(t, map[string]string{"shop.proto": header + `message A {}`}, "shop.proto")
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("wrote %d entries, want none", len(entries))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The committed example is what `gwag gen` writes today. This is the
// emitter's golden test and catches drift in either generator.
func TestExampleUpToDate(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := repoRoot(t)
	out := t.TempDir()
	written, err := Generate(context.Background(), Config{
		Lang:        "go",
		Out:         out,
		ImportPaths: []string{filepath.Join(root, "examples/events/proto")},
		Files:       []string{"orders/v1/orders.proto"},
		Opts:        []string{"paths=source_relative"},
		ModuleDir:   root,
		GwagVersion: "(devel)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(written, " ") != "orders/v1/orders.events.go orders/v1/orders.pb.go" {
		t.Fatalf("written = %v", written)
	}
	for _, w := range written {
		got, _ := os.ReadFile(filepath.Join(out, w))
		want, err := os.ReadFile(filepath.Join(root, "examples/events/gen", w))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from examples/events/gen; run examples/events/check.sh to regenerate", w)
		}
	}
}

func TestMissingToolNamesInstallCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/x\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeProtos(t, map[string]string{"shop.proto": header})
	_, err := Generate(context.Background(), Config{
		Lang: "go", Out: t.TempDir(), ImportPaths: []string{dir}, Files: []string{"shop.proto"}, ModuleDir: mod,
	})
	if err == nil || !strings.Contains(err.Error(), "go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@") {
		t.Fatalf("err = %v", err)
	}
}

func TestProtoName(t *testing.T) {
	dir := writeProtos(t, map[string]string{"a/b.proto": header})
	for _, f := range []string{filepath.Join(dir, "a/b.proto"), "a/b.proto"} {
		n, err := protoName([]string{dir}, f)
		if err != nil || n != "a/b.proto" {
			t.Errorf("protoName(%q) = %q, %v", f, n, err)
		}
	}
	if _, err := protoName([]string{filepath.Join(dir, "a")}, filepath.Join(dir, "x.proto")); err == nil {
		t.Error("missing file: no error")
	}
}

func TestCleanVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v2.5.0":                               "v2.5.0",
		"":                                     "(devel)",
		"(devel)":                              "(devel)",
		"v2.5.1-0.20261107004810-a2ef7c575d7c": "(devel)",
		"v2.5.0+dirty":                         "(devel)",
		"v2.6.0-rc.1":                          "v2.6.0-rc.1",
	} {
		if got := cleanVersion(in); got != want {
			t.Errorf("cleanVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Generated code for the edge shapes compiles and vets: a channel with
// no segments, enum / bytes / imported-message oneof cases, a nested
// union member.
func TestGoOutputCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds generated code")
	}
	root := repoRoot(t)
	rel := "gw/eventgen/testdata/compile"
	dir := filepath.Join(root, rel)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	src := writeProtos(t, map[string]string{"edge.proto": `syntax = "proto3";
package edge.v1;
import "gw/proto/ps/v1/options.proto";
import "google/protobuf/timestamp.proto";
option go_package = "github.com/iodesystems/gwag/v2/` + rel + `/edgev1;edgev1";
enum Level { LEVEL_UNSPECIFIED = 0; LEVEL_HIGH = 1; }
message Ping {
  option (gwag.ps.v1.binding) = { name: "Ping", pattern: "edge.ping" };
}
message Mixed {
  option (gwag.ps.v1.binding) = { name: "mixed", pattern: "edge.{order_id}.*.>" };
  oneof v {
    Level level = 1;
    bytes raw = 2;
    google.protobuf.Timestamp at = 3;
    double ratio = 4;
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
		Lang: "go", Out: dir, ImportPaths: []string{src}, Files: []string{"edge.proto"},
		Opts: []string{"module=github.com/iodesystems/gwag/v2/" + rel}, ModuleDir: root, GwagVersion: "(devel)",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "vet", "./"+rel+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "edgev1/edge.events.go"))
	ev := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		"type PingSubject struct{}",
		"OrderId string",
		"Rest string",
		"Level func(context.Context, MixedSubject, Level) error",
		"Raw func(context.Context, MixedSubject, []byte) error",
		"At func(context.Context, MixedSubject, *timestamppb.Timestamp) error",
		"Ratio func(context.Context, MixedSubject, float64) error",
		"Outer_Inner func(context.Context, NestSubject, *Outer_Inner) error",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
}
