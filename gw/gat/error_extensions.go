package gat

import "net/http"

// statusExtendedError wraps a handler error so the GraphQL surface exposes the
// originating HTTP status (and a string code) in the GraphQL error's
// `extensions`, while staying transparent to every other consumer: Error,
// Unwrap, and the GetStatus duck-type all delegate to the wrapped error.
//
// Why: over GraphQL every gat handler error otherwise comes back as a bare HTTP
// 400 with just a message, so a client can't tell a 401 from a 403 from a 500
// without string-matching. graphql-go's gqlerrors.FormatError copies
// Extensions() off the resolver error's OriginalError, so surfacing
// {status, code} here lets a client classify reliably (e.g. redirect to login
// on 401/403, show an error dialog on 5xx). gRPC/REST don't read extensions and
// are unaffected — they still see the same error + status.
type statusExtendedError struct {
	err    error
	status int
}

func (e *statusExtendedError) Error() string { return e.err.Error() }
func (e *statusExtendedError) Unwrap() error  { return e.err }
func (e *statusExtendedError) GetStatus() int { return e.status }

func (e *statusExtendedError) Extensions() map[string]any {
	return map[string]any{
		"status": e.status,
		"code":   httpStatusToConnectCode(e.status).String(),
	}
}

// withStatusExtensions wraps a non-nil handler error so the GraphQL error
// carries status/code extensions. An error that already exposes Extensions is
// returned as-is; one with no usable status defaults to 500 — matching gat's
// pre-existing "unrecognised → internal" behavior.
func withStatusExtensions(err error) error {
	if err == nil {
		return nil
	}
	type extended interface {
		Extensions() map[string]any
	}
	if _, ok := err.(extended); ok {
		return err
	}
	status := http.StatusInternalServerError
	type statusError interface{ GetStatus() int }
	if se, ok := err.(statusError); ok {
		status = se.GetStatus()
	}
	return &statusExtendedError{err: err, status: status}
}
