package main

import (
	"bytes"
	"strings"
	"testing"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func TestSecretGen(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := secretCmd([]string{"gen"}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	s := strings.TrimSpace(out.String())
	if err := gateway.CheckSecretStrength([]byte(s)); err != nil {
		t.Fatalf("generated %q refused: %v", s, err)
	}
	if code := secretCmd([]string{"nope"}, &out, &errOut); code != 2 {
		t.Errorf("bad usage code=%d", code)
	}
}

func TestDeclarationsSinkOptions_RefusesWeakSecret(t *testing.T) {
	_, err := declarationsSinkOptions("http://x", "changeme", "full", "", "full")
	if err == nil || !strings.Contains(err.Error(), "--declarations-hook-secret") || !strings.Contains(err.Error(), "openssl rand -base64 48") {
		t.Fatalf("err=%v", err)
	}
	s, _ := gateway.GenerateSecret()
	if _, err := declarationsSinkOptions("http://x", s, "full", "", "full"); err != nil {
		t.Fatalf("strong secret refused: %v", err)
	}
	if _, err := declarationsSinkOptions("http://x", "", "full", "", "full"); err != nil {
		t.Fatalf("no secret refused: %v", err)
	}
}
