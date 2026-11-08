package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployExportWritesFilesAndReusesToken(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	args := []string{"export", "--platform", "k8s", "--replicas", "3", "--out", dir}
	if rc := deployCmd(args, &out, &errb); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, errb.String())
	}
	tok1, err := os.ReadFile(filepath.Join(dir, "secrets", "admin-token"))
	if err != nil {
		t.Fatal(err)
	}
	if rc := deployCmd(args, &out, &errb); rc != 0 {
		t.Fatalf("second export rc=%d", rc)
	}
	tok2, _ := os.ReadFile(filepath.Join(dir, "secrets", "admin-token"))
	if string(tok1) != string(tok2) {
		t.Error("token rotated on re-export")
	}
	sec, _ := os.ReadFile(filepath.Join(dir, "secrets", "secret.yaml"))
	if !strings.Contains(string(sec), strings.TrimSpace(string(tok1))) {
		t.Error("secret.yaml lacks the token")
	}
}

func TestDeployRejectsBadReplicasAndPlatform(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := deployCmd([]string{"export", "--platform", "k8s", "--replicas", "0", "--out", t.TempDir()}, &out, &errb); rc == 0 {
		t.Error("replicas 0 accepted")
	}
	if rc := deployCmd([]string{"up", "--platform", "ecs"}, &out, &errb); rc == 0 {
		t.Error("deploy up --platform ecs accepted")
	}
	errb.Reset()
	deployCmd([]string{"export", "--platform", "compose", "--replicas", "2", "--out", t.TempDir()}, &out, &errb)
	if !strings.Contains(errb.String(), "warning") {
		t.Error("replicas 2 gave no warning")
	}
}

func TestAdminTokenOption(t *testing.T) {
	t.Setenv("GWAG_ADMIN_TOKEN", "")
	if o, err := adminTokenOption(""); o != nil || err != nil {
		t.Errorf("unset: %v %v", o, err)
	}
	t.Setenv("GWAG_ADMIN_TOKEN", strings.Repeat("ab", 32))
	if o, err := adminTokenOption(""); o == nil || err != nil {
		t.Errorf("env: %v %v", o, err)
	}
	t.Setenv("GWAG_ADMIN_TOKEN", "zz")
	if _, err := adminTokenOption(""); err == nil {
		t.Error("non-hex accepted")
	}
	t.Setenv("GWAG_ADMIN_TOKEN", "abcd")
	if _, err := adminTokenOption(""); err == nil {
		t.Error("short token accepted")
	}
}
