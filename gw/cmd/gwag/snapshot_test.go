package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func snapRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := schemaSnapshotCmd(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeTmp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sdlBase = `type Query { user(id: ID!): User  users: [User] }
type User { id: ID! name: String }
`

func TestSnapshotGraphQL(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "v1.graphql")
	gen := writeTmp(t, dir, "gen.graphql", sdlBase)

	// Missing snapshot: update creates it.
	if c, _, e := snapRun(t, "update", "--spec", gen, "--snapshot", snap); c != 0 {
		t.Fatalf("create: %d %s", c, e)
	}
	// Reordered, reformatted spec is equal.
	reordered := writeTmp(t, dir, "re.graphql", `type User { name: String
 id: ID! }
type Query { users: [User]
 user(id: ID!): User }`)
	if c, o, e := snapRun(t, "check", "--spec", reordered, "--snapshot", snap); c != snapExitEqual {
		t.Fatalf("equal: %d %s %s", c, o, e)
	}
	// Additive.
	add := writeTmp(t, dir, "add.graphql", sdlBase+"type Extra { a: String }\n")
	c, o, _ := snapRun(t, "check", "--spec", add, "--snapshot", snap)
	if c != snapExitAdditive || !strings.Contains(o, "[INFO] type added: Extra") || !strings.Contains(o, "+ type Extra") {
		t.Fatalf("additive: %d\n%s", c, o)
	}
	// Breaking.
	brk := writeTmp(t, dir, "brk.graphql", `type Query { user(id: ID!): User }
type User { id: ID! }
`)
	c, o, _ = snapRun(t, "check", "--spec", brk, "--snapshot", snap)
	if c != snapExitBreaking || !strings.Contains(o, "[BREAKING] field removed: User.name") {
		t.Fatalf("breaking: %d\n%s", c, o)
	}
	// Update refuses breaking, snapshot untouched.
	before, _ := os.ReadFile(snap)
	c, _, e := snapRun(t, "update", "--spec", brk, "--snapshot", snap)
	after, _ := os.ReadFile(snap)
	if c != snapExitBreaking || !bytes.Equal(before, after) || !strings.Contains(e, "refusing") {
		t.Fatalf("update breaking: %d %s", c, e)
	}
	// Update accepts additive.
	if c, _, e := snapRun(t, "update", "--spec", add, "--snapshot", snap); c != 0 {
		t.Fatalf("update additive: %d %s", c, e)
	}
	if c, _, _ := snapRun(t, "check", "--spec", add, "--snapshot", snap); c != 0 {
		t.Fatalf("check after update: %d", c)
	}
	// Override.
	if c, _, e := snapRun(t, "update", "--spec", brk, "--snapshot", snap, "--allow-breaking"); c != 0 {
		t.Fatalf("allow-breaking: %d %s", c, e)
	}
}

func TestSnapshotOpenAPI(t *testing.T) {
	dir := t.TempDir()
	base := `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/a":{"get":{"description":"x","parameters":[{"name":"q","in":"query"}],"responses":{"200":{"description":"ok"}}}}}}`
	snap := writeTmp(t, dir, "v1.json", base)
	// YAML spec with the same content is equal.
	yml := writeTmp(t, dir, "gen.yaml", `
paths:
  /a:
    get:
      responses: {"200": {description: ok}}
      parameters: [{in: query, name: q}]
      description: x
info: {version: "1", title: t}
openapi: 3.0.0
`)
	if c, o, e := snapRun(t, "check", "--spec", yml, "--snapshot", snap, "--format", "openapi"); c != 0 {
		t.Fatalf("equal: %d %s %s", c, o, e)
	}
	// Added optional param and new path: additive; doc change: info.
	add := writeTmp(t, dir, "add.json", strings.Replace(strings.Replace(base,
		`"parameters":[{"name":"q","in":"query"}]`, `"parameters":[{"name":"q","in":"query"},{"name":"r","in":"query"}]`, 1),
		`"description":"x"`, `"description":"y"`, 1))
	c, o, _ := snapRun(t, "check", "--spec", add, "--snapshot", snap, "--format", "openapi")
	if c != snapExitAdditive || !strings.Contains(o, "[INFO] added: /paths//a/get/parameters[query:r]") || !strings.Contains(o, "doc changed") {
		t.Fatalf("additive: %d\n%s", c, o)
	}
	// New required parameter: breaking.
	req := writeTmp(t, dir, "req.json", strings.Replace(base,
		`"parameters":[{"name":"q","in":"query"}]`, `"parameters":[{"name":"q","in":"query"},{"name":"r","in":"query","required":true}]`, 1))
	if c, o, _ := snapRun(t, "check", "--spec", req, "--snapshot", snap, "--format", "openapi"); c != snapExitBreaking {
		t.Fatalf("required param: %d\n%s", c, o)
	}
	// Removed path: breaking, update refuses.
	gone := writeTmp(t, dir, "gone.json", `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{}}`)
	c, o, _ = snapRun(t, "update", "--spec", gone, "--snapshot", snap, "--format", "openapi")
	if c != snapExitBreaking || !strings.Contains(o, "[BREAKING] removed: /paths//a") {
		t.Fatalf("removed: %d\n%s", c, o)
	}
}

const protoBase = `syntax = "proto3";
package p.v1;
message A { string a = 1; }
service S { rpc Get(A) returns (A); }
`

func TestSnapshotProto(t *testing.T) {
	dir := t.TempDir()
	src := writeTmp(t, dir, "p.proto", protoBase)
	snap := filepath.Join(dir, "p.snapshot.json")
	if c, _, e := snapRun(t, "update", "--spec", src, "--snapshot", snap, "--format", "proto"); c != 0 {
		t.Fatalf("create: %d %s", c, e)
	}
	// Comments and whitespace do not matter.
	writeTmp(t, dir, "p.proto", "// c\n"+protoBase+"\n")
	if c, o, e := snapRun(t, "check", "--spec", src, "--snapshot", snap, "--format", "proto"); c != 0 {
		t.Fatalf("equal: %d %s %s", c, o, e)
	}
	// New field: additive.
	writeTmp(t, dir, "p.proto", strings.Replace(protoBase, "string a = 1;", "string a = 1; string b = 2;", 1))
	c, o, _ := snapRun(t, "check", "--spec", src, "--snapshot", snap, "--format", "proto")
	if c != snapExitAdditive || !strings.Contains(o, "[INFO] added: /message_type[A]/field[b]") {
		t.Fatalf("additive: %d\n%s", c, o)
	}
	// Renumbered field: breaking.
	writeTmp(t, dir, "p.proto", strings.Replace(protoBase, "a = 1", "a = 2", 1))
	c, o, _ = snapRun(t, "check", "--spec", src, "--snapshot", snap, "--format", "proto")
	if c != snapExitBreaking || !strings.Contains(o, "[BREAKING] changed: /message_type[A]/field[a]/number") {
		t.Fatalf("breaking: %d\n%s", c, o)
	}
}

func TestSnapshotUsage(t *testing.T) {
	if c, _, _ := snapRun(t); c != 2 {
		t.Fatalf("no args: %d", c)
	}
	if c, _, _ := snapRun(t, "check", "--spec", "x.unknown", "--snapshot", "y.unknown"); c != 2 {
		t.Fatalf("no format: %d", c)
	}
}
