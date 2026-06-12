package gat

import (
	"errors"
	"net/http"
	"testing"
)

func TestWithStatusExtensions_StatusError(t *testing.T) {
	wrapped := withStatusExtensions(&hsErr{status: http.StatusUnauthorized, msg: "authentication required"})

	if wrapped.Error() != "authentication required" {
		t.Errorf("Error() = %q; want preserved message", wrapped.Error())
	}
	ext, ok := wrapped.(interface{ Extensions() map[string]any })
	if !ok {
		t.Fatalf("wrapped error does not expose Extensions()")
	}
	x := ext.Extensions()
	if x["status"] != http.StatusUnauthorized {
		t.Errorf("extensions status = %v; want 401", x["status"])
	}
	if x["code"] != "unauthenticated" {
		t.Errorf("extensions code = %v; want unauthenticated", x["code"])
	}
	// Transparent to the gRPC path: GetStatus still resolves.
	se, ok := wrapped.(interface{ GetStatus() int })
	if !ok || se.GetStatus() != http.StatusUnauthorized {
		t.Errorf("GetStatus not preserved through wrap")
	}
	// Unwrap reaches the original error.
	if !errors.Is(wrapped, wrapped) {
		t.Errorf("errors.Is sanity failed")
	}
}

func TestWithStatusExtensions_PlainErrorDefaultsTo500(t *testing.T) {
	wrapped := withStatusExtensions(errors.New("boom"))
	x := wrapped.(interface{ Extensions() map[string]any }).Extensions()
	if x["status"] != http.StatusInternalServerError {
		t.Errorf("plain error status = %v; want 500", x["status"])
	}
	if x["code"] != "internal" {
		t.Errorf("plain error code = %v; want internal", x["code"])
	}
}

func TestWithStatusExtensions_NilAndIdempotent(t *testing.T) {
	if withStatusExtensions(nil) != nil {
		t.Errorf("nil error should stay nil")
	}
	once := withStatusExtensions(&hsErr{status: http.StatusForbidden, msg: "nope"})
	twice := withStatusExtensions(once)
	if once != twice {
		t.Errorf("already-extended error should be returned as-is (not re-wrapped)")
	}
	if x := twice.(interface{ Extensions() map[string]any }).Extensions(); x["code"] != "permission_denied" {
		t.Errorf("403 code = %v; want permission_denied", x["code"])
	}
}
