package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iodesystems/gwag/v2/events"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/events/schemaregistry/srtest"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
)

const corpMoney = "syntax = \"proto3\";\npackage corp.common.v1;\noption go_package = \"example.com/corp/commonv1\";\nmessage Money { int64 cents = 1; string currency = 2; }\n"

const corpOrders = "syntax = \"proto3\";\npackage corp.orders.v1;\nimport \"corp/common/v1/money.proto\";\n" +
	"option go_package = \"example.com/corp/ordersv1\";\n" +
	"message Order {\n  string order_id = 1;\n  corp.common.v1.Money total = 2;\n  Status status = 3;\n  repeated string skus = 4;\n" +
	"  enum Status { STATUS_UNSPECIFIED = 0; PLACED = 1; SHIPPED = 2; }\n}\n"

func corpRegistry(t *testing.T) *srtest.Registry {
	t.Helper()
	r := srtest.New()
	t.Cleanup(r.Close)
	_, mv := r.Add("corp/common/v1/money.proto", sr.Schema{Schema: corpMoney})
	r.Add("orders-value", sr.Schema{Schema: corpOrders, References: []sr.Reference{
		{Name: "corp/common/v1/money.proto", Subject: "corp/common/v1/money.proto", Version: mv},
	}})
	return r
}

func run(t *testing.T, f func([]string, *bytes.Buffer, *bytes.Buffer) int, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := f(args, &out, &errb)
	return code, out.String(), errb.String()
}

func gen(args []string, out, errb *bytes.Buffer) int   { return genCmd(args, out, errb) }
func check(args []string, out, errb *bytes.Buffer) int { return schemaCheckCmd(args, out, errb) }
func push(args []string, out, errb *bytes.Buffer) int  { return schemaPushCmd(args, out, errb) }

// import: gen fetches the subject and its reference, writes read-only
// copies and a lock, and generates a typed channel for the first
// message; --check-lock compares the lock with the registry and disk.
func TestGenImport(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	r := corpRegistry(t)
	root, _ := filepath.Abs("../../..")
	dir := t.TempDir()
	imp := filepath.Join(dir, "imported")
	code, out, errs := run(t, gen, "--registry", r.URL, "--subject", "orders-value", "--import-dir", imp,
		"--lang", "go", "--out", filepath.Join(dir, "gen"), "--module-dir", root, "--opt", "paths=source_relative")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !strings.Contains(out, `imported orders-value version 1 id 2 -> `+filepath.Join(imp, "registry/orders-value.proto")+` channel corp.orders.v1.Order pattern "orders"`) {
		t.Fatalf("stdout = %q", out)
	}
	for _, f := range []string{"gen/registry/orders-value.events.go", "gen/registry/orders-value.pb.go", "gen/corp/common/v1/money.pb.go", "imported/corp/common/v1/money.proto"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := os.ReadFile(filepath.Join(imp, "registry/orders-value.proto"))
	if !strings.HasPrefix(string(src), "// Imported from "+r.URL+" subject orders-value version 1 id 2; do not edit.") {
		t.Fatalf("header: %q", src[:120])
	}
	ev, _ := os.ReadFile(filepath.Join(dir, "gen/registry/orders-value.events.go"))
	if !strings.Contains(string(ev), `events.MustChannel("corp.orders.v1.Order", "orders")`) {
		t.Fatal("events file has no Order channel")
	}
	var l lockFile
	b, _ := os.ReadFile(filepath.Join(imp, "gwag.lock.json"))
	if err := json.Unmarshal(b, &l); err != nil || len(l.Subjects) != 1 || len(l.Subjects[0].References) != 1 || l.Subjects[0].ID != 2 {
		t.Fatalf("lock %s: %v", b, err)
	}

	// The committed copy regenerates without the registry.
	before := r.Total()
	if err := os.RemoveAll(filepath.Join(dir, "gen")); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, gen, "--import-dir", imp, "--lang", "go", "--out", filepath.Join(dir, "gen"),
		"--module-dir", root, "--opt", "paths=source_relative"); code != 0 {
		t.Fatalf("offline regen exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen/registry/orders-value.events.go")); err != nil {
		t.Fatal(err)
	}
	if n := r.Total() - before; n != 0 {
		t.Fatalf("offline regen made %d registry requests", n)
	}

	if code, out, errs := run(t, gen, "--check-lock", "--import-dir", imp); code != 0 || !strings.HasPrefix(out, "ok") {
		t.Fatalf("check-lock exit %d: %s %s", code, out, errs)
	}
	// A new version upstream: the lock pins, so only a note.
	r.Add("orders-value", sr.Schema{Schema: corpOrders + "message Extra {}\n", References: []sr.Reference{
		{Name: "corp/common/v1/money.proto", Subject: "corp/common/v1/money.proto", Version: 1},
	}})
	if code, _, errs := run(t, gen, "--check-lock", "--import-dir", imp); code != 0 || !strings.Contains(errs, "version 1 is locked; the registry's latest is 2") {
		t.Fatalf("check-lock exit %d: %s", code, errs)
	}
	// An edited copy fails.
	p := filepath.Join(imp, "corp/common/v1/money.proto")
	_ = os.Chmod(p, 0o644)
	if err := os.WriteFile(p, append(src, "// edit\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, gen, "--check-lock", "--import-dir", imp); code != 1 || !strings.Contains(errs, "FAIL corp/common/v1/money.proto: file was edited") {
		t.Fatalf("check-lock after edit: exit %d: %s", code, errs)
	}
	// Re-import from the lock restores the pinned version.
	if code, _, errs := run(t, gen, "--registry", r.URL, "--import-dir", imp); code != 0 {
		t.Fatalf("re-import exit %d: %s", code, errs)
	}
	if code, _, errs := run(t, gen, "--check-lock", "--import-dir", imp); code != 0 {
		t.Fatalf("check-lock after re-import: exit %d: %s", code, errs)
	}
	// --bind overrides the channel; a subject that is not <topic>-value needs a pattern.
	r.Add("corp.orders.v1.Order", sr.Schema{Schema: corpOrders, References: []sr.Reference{
		{Name: "corp/common/v1/money.proto", Subject: "corp/common/v1/money.proto", Version: 1},
	}})
	imp2 := filepath.Join(dir, "imp2")
	if code, _, errs := run(t, gen, "--registry", r.URL, "--subject", "corp.orders.v1.Order", "--import-dir", imp2); code != 1 || !strings.Contains(errs, "give the pattern with --bind") {
		t.Fatalf("record subject: exit %d: %s", code, errs)
	}
	if code, out, errs := run(t, gen, "--registry", r.URL, "--subject", "corp.orders.v1.Order@1", "--import-dir", imp2,
		"--bind", "corp.orders.v1.Order=corp.orders.v1.Order,name=Orders,pattern=corp.orders"); code != 0 || !strings.Contains(out, `channel corp.orders.v1.Orders pattern "corp.orders"`) {
		t.Fatalf("bind: exit %d: %s %s", code, out, errs)
	}
}

const shadowProto = `syntax = "proto3";
package shop.v1;
import "gw/proto/ps/v1/options.proto";
message Money { int64 cents = 1; }
message Order {
  option (gwag.ps.v1.shadow) = { registry: "corp", subject: "orders-value" };
  string order_id = 1;
  Money total = 2;
}
message Bad {
  option (gwag.ps.v1.shadow) = { registry: "corp", subject: "orders-value", version: "1", message: "corp.orders.v1.Order" };
  int32 order_id = 1;
  string extra = 9;
}
message Out {
  option (gwag.ps.v1.shadow) = { registry: "corp", subject: "orders-value", message: "corp.orders.v1.Order", side: PRODUCER };
  string order_id = 1;
}
`

func TestSchemaCheck(t *testing.T) {
	r := corpRegistry(t)
	var asked []string
	r.Compatible = func(subject, version string, s sr.Schema) (bool, []string) {
		asked = append(asked, subject+"@"+version)
		return false, []string{`{errorType:"FIELD_REMOVED"}`, "{oldSchemaVersion: 1}", "{oldSchema: '...'}"}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shadow.proto"), []byte(shadowProto), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, check, "--registry", "corp="+r.URL, "-I", dir, "shadow.proto")
	if code != 1 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	want := `ok   shop.v1.Order (consumer) vs corp orders-value@latest
FAIL shop.v1.Bad (consumer) vs corp orders-value@1
  error   order_id: field 1 is int32, registry has string
  error   extra: field 9 is not in registry message corp.orders.v1.Order
FAIL shop.v1.Out (producer) vs corp orders-value@latest
  error   registry (BACKWARD): {errorType:"FIELD_REMOVED"}
`
	if out != want {
		t.Fatalf("stdout\n%s\nwant\n%s", out, want)
	}
	if len(asked) != 1 || asked[0] != "orders-value@latest" {
		t.Fatalf("compatibility asked %v", asked)
	}
	if code, out, _ := run(t, check, "-I", dir, "shadow.proto"); code != 1 || !strings.Contains(out, "FAIL shop.v1.Order (consumer) vs corp orders-value@latest: no --registry corp=URL") {
		t.Fatalf("no registry: exit %d %s", code, out)
	}
}

// own: push registers what the Serde looks up at publish, so a
// lookup-only Serde finds the pushed schema.
func TestSchemaPush(t *testing.T) {
	r := srtest.New()
	defer r.Close()
	root, _ := filepath.Abs("../../..")
	code, out, errs := run(t, push, "--registry", r.URL, "-I", filepath.Join(root, "examples/events/proto"), "orders/v1/orders.proto")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.Contains(out, "ok   orders.v1.OrderEvents subject EV_orders_v1_OrderEvents-value version 1 id 2") {
		t.Fatalf("stdout = %q", out)
	}
	pushed := r.Requests("POST /subjects/EV_orders_v1_OrderEvents-value/versions")
	c, _ := sr.NewClient(sr.Config{URL: r.URL})
	msg := &events.Message{Headers: map[string]string{}}
	ch := events.MustChannel("orders.v1.OrderEvents", "events.orders.{orderId}.lifecycle")
	serde, err := sr.NewSerde(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := serde.Encode(context.Background(), ch, ch.Stream, &ordersv1.OrderCreated{}, msg); err != nil {
		t.Fatalf("lookup-only encode after push: %v", err)
	}
	if id, _, _, _ := sr.Unframe(msg.Data); id != 2 {
		t.Fatalf("id %d", id)
	}
	if n := r.Requests("POST /subjects/EV_orders_v1_OrderEvents-value/versions"); n != pushed {
		t.Fatalf("lookup-only Serde registered (%d)", n)
	}
	if code, _, _ := run(t, push, "--dry-run", "--registry", r.URL, "--topic", "orders.v1.Audit=audit", "-I", filepath.Join(root, "examples/events/proto"), "orders/v1/orders.proto"); code != 1 {
		t.Fatalf("dry-run of an unpushed topic: exit %d", code)
	}
}
