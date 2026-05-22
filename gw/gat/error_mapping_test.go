package gat

// Coverage for handlerErrorToConnect — the dispatch path's
// HTTP-status → Connect-code mapping. Without this, huma 4xx errors
// (NotFound, Conflict, etc.) come out the wire as CodeInternal,
// which presents as "internal:" to CLI tooling and misclassifies
// what was really a clean client-side problem.

import (
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
)

// hsErr is a minimal StatusError implementer for the duck-typed
// shape we care about.
type hsErr struct {
	status int
	msg    string
}

func (e *hsErr) Error() string  { return e.msg }
func (e *hsErr) GetStatus() int { return e.status }

func TestHandlerErrorToConnect_NotFound(t *testing.T) {
	got := handlerErrorToConnect(&hsErr{status: http.StatusNotFound, msg: "missing"})
	if got.Code() != connect.CodeNotFound {
		t.Errorf("404 → %v; want CodeNotFound", got.Code())
	}
	if got.Message() != "missing" {
		t.Errorf("message = %q; want %q", got.Message(), "missing")
	}
}

func TestHandlerErrorToConnect_Conflict(t *testing.T) {
	got := handlerErrorToConnect(&hsErr{status: http.StatusConflict, msg: "dup"})
	if got.Code() != connect.CodeAlreadyExists {
		t.Errorf("409 → %v; want CodeAlreadyExists", got.Code())
	}
}

func TestHandlerErrorToConnect_TooManyRequests(t *testing.T) {
	got := handlerErrorToConnect(&hsErr{status: http.StatusTooManyRequests, msg: "slow down"})
	if got.Code() != connect.CodeResourceExhausted {
		t.Errorf("429 → %v; want CodeResourceExhausted", got.Code())
	}
}

func TestHandlerErrorToConnect_GenericBadRequest(t *testing.T) {
	// 422 isn't in the explicit switch table — should still resolve
	// to a sensible CodeInvalidArgument via the 4xx fallthrough.
	got := handlerErrorToConnect(&hsErr{status: 422, msg: "validation"})
	if got.Code() != connect.CodeInvalidArgument {
		t.Errorf("422 → %v; want CodeInvalidArgument", got.Code())
	}
}

func TestHandlerErrorToConnect_GenericServerError(t *testing.T) {
	got := handlerErrorToConnect(&hsErr{status: 599, msg: "weird"})
	if got.Code() != connect.CodeInternal {
		t.Errorf("599 → %v; want CodeInternal", got.Code())
	}
}

// Plain Go errors with no GetStatus method keep landing on CodeInternal
// — preserves the pre-mapping default for the cases we couldn't class
// (panics caught upstream, repo errors, etc.).
func TestHandlerErrorToConnect_PlainErrorIsInternal(t *testing.T) {
	got := handlerErrorToConnect(errors.New("boom"))
	if got.Code() != connect.CodeInternal {
		t.Errorf("plain → %v; want CodeInternal", got.Code())
	}
}
