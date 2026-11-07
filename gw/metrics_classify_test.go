package gateway

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyError must emit the grpc codes.Code.String() form for every
// path, so one PromQL selector counts a failure class across kinds.
func TestClassifyErrorCanonicalSpelling(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"grpc internal", status.Error(codes.Internal, "x"), "Internal"},
		{"grpc deadline", status.Error(codes.DeadlineExceeded, "x"), "DeadlineExceeded"},
		{"reject internal", Reject(CodeInternal, "x"), "Internal"},
		{"reject unauthenticated", Reject(CodeUnauthenticated, "x"), "Unauthenticated"},
		{"reject permission denied", Reject(CodePermissionDenied, "x"), "PermissionDenied"},
		{"reject resource exhausted", Reject(CodeResourceExhausted, "x"), "ResourceExhausted"},
		{"reject invalid argument", Reject(CodeInvalidArgument, "x"), "InvalidArgument"},
		{"reject not found", Reject(CodeNotFound, "x"), "NotFound"},
		{"wrapped reject", fmt.Errorf("wrap: %w", Reject(CodeInternal, "x")), "Internal"},
		{"plain error", errors.New("boom"), "Internal"},
	}
	for _, c := range cases {
		if got := classifyError(c.err); got != c.want {
			t.Errorf("%s: classifyError = %q, want %q", c.name, got, c.want)
		}
	}
}
