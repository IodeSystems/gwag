package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type statusErr int

func (e statusErr) Error() string  { return fmt.Sprintf("http %d", int(e)) }
func (e statusErr) GetStatus() int { return int(e) }

// One error, one status on REST ingress. before is the status the
// REST ingress answered before transport rejections kept their cause
// and gat.ErrorCode checked context errors ahead of GetStatus; want is
// the status now. Rows where they differ are listed in CHANGELOG.md.
func TestHTTPIngress_ErrorStatusTable(t *testing.T) {
	transport := func(cause error) error {
		return rejectUpstream(upstreamClassTransport, CodeInternal, "openapi: GET /x: "+cause.Error(), cause)
	}
	for _, tc := range []struct {
		name   string
		err    error
		before int
		want   int
	}{
		{"reject unauthenticated", Reject(CodeUnauthenticated, "x"), 401, 401},
		{"reject permission denied", Reject(CodePermissionDenied, "x"), 403, 403},
		{"reject resource exhausted", Reject(CodeResourceExhausted, "x"), 429, 429},
		{"reject invalid argument", Reject(CodeInvalidArgument, "x"), 400, 400},
		{"reject not found", Reject(CodeNotFound, "x"), 404, 404},
		{"reject internal", Reject(CodeInternal, "x"), 500, 500},
		{"context deadline", context.DeadlineExceeded, 504, 504},
		{"context canceled", context.Canceled, 499, 499},
		{"wrapped deadline", fmt.Errorf("dispatch: %w", context.DeadlineExceeded), 504, 504},
		{"wrapped canceled", fmt.Errorf("dispatch: %w", context.Canceled), 499, 499},
		{"transport failure", transport(errors.New("connection refused")), 500, 500},
		{"transport deadline", transport(context.DeadlineExceeded), 500, 504},
		{"transport canceled", transport(context.Canceled), 500, 499},
		{"plain error", errors.New("boom"), 500, 500},
		{"error with http status 409", statusErr(409), 409, 409},
		{"error with http status 422", statusErr(422), 400, 400},
		{"upstream 400", rejectUpstreamStatus(400, "x"), 400, 400},
		{"upstream 409", rejectUpstreamStatus(409, "x"), 409, 409},
		{"upstream 410", rejectUpstreamStatus(410, "x"), 410, 410},
		{"upstream 418", rejectUpstreamStatus(418, "x"), 418, 418},
		{"upstream 502", rejectUpstreamStatus(502, "x"), 502, 502},
		{"upstream 503", rejectUpstreamStatus(503, "x"), 503, 503},
		{"upstream 504", rejectUpstreamStatus(504, "x"), 504, 504},
		{"grpc InvalidArgument", status.Error(codes.InvalidArgument, "x"), 400, 400},
		{"grpc NotFound", status.Error(codes.NotFound, "x"), 404, 404},
		{"grpc AlreadyExists", status.Error(codes.AlreadyExists, "x"), 409, 409},
		{"grpc FailedPrecondition", status.Error(codes.FailedPrecondition, "x"), 400, 400},
		{"grpc Aborted", status.Error(codes.Aborted, "x"), 409, 409},
		{"grpc ResourceExhausted", status.Error(codes.ResourceExhausted, "x"), 429, 429},
		{"grpc Unimplemented", status.Error(codes.Unimplemented, "x"), 501, 501},
		{"grpc Unavailable", status.Error(codes.Unavailable, "x"), 503, 503},
		{"grpc DeadlineExceeded", status.Error(codes.DeadlineExceeded, "x"), 504, 504},
		{"grpc Canceled", status.Error(codes.Canceled, "x"), 499, 499},
		{"grpc Unauthenticated", status.Error(codes.Unauthenticated, "x"), 401, 401},
		{"grpc PermissionDenied", status.Error(codes.PermissionDenied, "x"), 403, 403},
		{"grpc Internal", status.Error(codes.Internal, "x"), 500, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeIngressDispatchError(w, tc.err)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d (before: %d)", w.Code, tc.want, tc.before)
			}
		})
	}
}

// A quota rejection keeps its Retry-After header next to the status.
func TestHTTPIngress_RetryAfterWithStatus(t *testing.T) {
	w := httptest.NewRecorder()
	writeIngressDispatchError(w, &rejection{Code: CodeResourceExhausted, Msg: "q", RetryAfter: 1500 * time.Millisecond})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("status %d Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}
