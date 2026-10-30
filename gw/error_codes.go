package gateway

import (
	"errors"
	"time"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// connectCodedError gives a resolver error gat's GraphQL extensions
// shape, {status, code}, under WithConnectErrorCodes. Error, Unwrap and
// GetStatus delegate, so every other reader sees the wrapped error.
type connectCodedError struct {
	err error
}

func (e *connectCodedError) Error() string { return e.err.Error() }
func (e *connectCodedError) Unwrap() error { return e.err }

// GetStatus is the wrapped error's HTTP status when it has one, else
// the status of its gRPC code.
func (e *connectCodedError) GetStatus() int {
	type statusError interface{ GetStatus() int }
	var se statusError
	if errors.As(e.err, &se) {
		return se.GetStatus()
	}
	return gat.HTTPStatus(gat.ErrorCode(e.err))
}

func (e *connectCodedError) Extensions() map[string]any {
	ext := map[string]any{
		"status": e.GetStatus(),
		"code":   gat.ErrorCode(e.err).String(),
	}
	var rej *rejection
	if errors.As(e.err, &rej) && rej.RetryAfter > 0 {
		ext["retryAfterSeconds"] = int(rej.RetryAfter / time.Second)
	}
	return ext
}

// connectErrorCodes is the ir.RuntimeOptions.WrapError gwag installs
// under WithConnectErrorCodes. A Reject error is reshaped; an error
// that carries other extensions (a downstream GraphQL error, a
// subscription auth failure) keeps them.
func connectErrorCodes(err error) error {
	var rej *rejection
	if !errors.As(err, &rej) {
		if _, ok := err.(interface{ Extensions() map[string]any }); ok {
			return err
		}
	}
	return &connectCodedError{err: err}
}
