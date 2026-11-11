package main

import "testing"

func TestSelfAdminURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":           "http://localhost:8080/api",
		"127.0.0.1:28500": "http://127.0.0.1:28500/api",
		"0.0.0.0:8080":    "http://0.0.0.0:8080/api",
	} {
		if got := selfAdminURL(addr); got != want {
			t.Errorf("selfAdminURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
