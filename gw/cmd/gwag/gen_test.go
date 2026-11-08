package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenCmdUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := genCmd(nil, &out, &errb); code != 2 {
		t.Fatalf("no args: exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Usage: gwag gen") {
		t.Fatalf("stderr = %q", errb.String())
	}
	errb.Reset()
	if code := genCmd([]string{"--lang", "cobol", "x.proto"}, &out, &errb); code != 1 {
		t.Fatalf("unknown lang: exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), `unknown language "cobol"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// Flags may come after the file names.
func TestGenCmdGo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root, _ := filepath.Abs("../../..")
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := genCmd([]string{
		"--lang", "go", "orders/v1/orders.proto",
		"-I", filepath.Join(root, "examples/events/proto"),
		"--out", out, "--opt", "paths=source_relative", "--module-dir", root,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "orders/v1/orders.events.go\norders/v1/orders.pb.go\n" {
		t.Fatalf("stdout = %q", got)
	}
	if _, err := os.Stat(filepath.Join(out, "orders/v1/orders.events.go")); err != nil {
		t.Fatal(err)
	}
}
