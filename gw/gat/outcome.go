package gat

import (
	"context"
	"net/http"
	"strings"

	"github.com/IodeSystems/graphql-go/v2/gqlerrors"

	"github.com/iodesystems/gwag/v2/gw/internal/gqlguard"
)

// Outcome is how gat answered one GraphQL operation (one per document;
// a batch reports each). A host reads it to count refusals and errors.
//
// Stability: experimental
type Outcome struct {
	// Reject names why the operation was refused before it ran, or is
	// empty. One of: max_request_bytes, malformed_body, missing_query,
	// persisted_unsupported, trusted_required, trusted_unknown,
	// max_operations, max_depth, introspection_disabled,
	// mutation_over_get.
	Reject string
	// ErrorKind is parse, validate or execution when the response
	// carries errors, else empty. Refusals by a request limit are
	// reported through Reject, not here.
	ErrorKind string
	// Partial is set when the response carries both data and errors.
	Partial bool
	// Mutation, Subscription: the operation type; neither means query
	// (or unknown, when it did not plan).
	Mutation, Subscription bool
}

// OutcomeObserver receives an Outcome. ctx is the one the PreRequest
// hook returned (the connection's, for WebSocket subscriptions).
//
// Stability: experimental
type OutcomeObserver func(ctx context.Context, o Outcome)

// ObserveOutcome sets the outcome observer (nil: off, the default;
// nothing is computed when off). Set before serving. Returns g for
// chaining.
//
// Stability: experimental
func (g *Gateway) ObserveOutcome(fn OutcomeObserver) *Gateway {
	g.outcomeObs = fn
	return g
}

func (g *Gateway) reject(ctx context.Context, reason string) {
	if g.outcomeObs != nil {
		g.outcomeObs(ctx, Outcome{Reject: reason})
	}
}

// tooLarge answers 413 and reports it.
func (g *Gateway) tooLarge(ctx context.Context, w http.ResponseWriter) {
	g.reject(ctx, "max_request_bytes")
	writeTooLarge(w)
}

// badRequest answers 400 with msg and reports reason.
func (g *Gateway) badRequest(ctx context.Context, w http.ResponseWriter, msg, reason string) {
	g.reject(ctx, reason)
	writeError(w, msg, http.StatusBadRequest)
}

// rejectReason maps a refused plan (status != 0) to its reason.
func rejectReason(code string) string {
	switch code {
	case "PERSISTED_QUERY_NOT_SUPPORTED":
		return "persisted_unsupported"
	case "PERSISTED_QUERY_NOT_FOUND":
		return "trusted_unknown"
	case "TRUSTED_DOCUMENT_REQUIRED":
		return "trusted_required"
	}
	return "missing_query"
}

// planErrorOutcome classifies a plan's errors: a request-limit rule
// is a refusal; a syntax error is parse; anything else validate.
func planErrorOutcome(errs []gqlerrors.FormattedError) Outcome {
	kind := "validate"
	for _, e := range errs {
		if r := gqlguard.LimitReason(e.Message); r != "" {
			return Outcome{Reject: r}
		}
		if strings.HasPrefix(e.Message, "Syntax Error") {
			kind = "parse"
		}
	}
	return Outcome{ErrorKind: kind}
}

// payloadOutcome reads a response payload from execution. Errors come
// last in the JSON ({"data":...,"errors":[...]}), so a payload ends in
// "]}" exactly when it has them: data is an object or null, which end
// in "}" and "l".
func payloadOutcome(payload []byte) (hasErrors, partial bool) {
	n := len(payload)
	if n < 2 || payload[n-2] != ']' || payload[n-1] != '}' {
		return false, false
	}
	s := string(payload[:min(n, 16)])
	return true, strings.HasPrefix(s, `{"data":`) && !strings.HasPrefix(s, `{"data":null`)
}

// observePlanned reports a request's outcome once it has been planned
// and, when it ran, executed. ranPayload is nil for a request that did
// not run.
func (g *Gateway) observePlanned(ctx context.Context, p plannedRequest, payload []byte) {
	if g.outcomeObs == nil {
		return
	}
	var o Outcome
	switch {
	case p.status != 0:
		o.Reject = rejectReason(p.code)
	case len(p.pr.Errors) > 0:
		o = planErrorOutcome(p.pr.Errors)
	default:
		if p.pr.Plan != nil {
			o.Mutation = p.pr.Plan.IsMutation()
		}
		if hasErr, partial := payloadOutcome(payload); hasErr {
			o.ErrorKind, o.Partial = "execution", partial
		}
	}
	g.outcomeObs(ctx, o)
}

// observeEvent reports a subscription result that carries errors: no
// data means the subscription did not start (parse / validate), data
// plus errors is a partial event.
func (g *Gateway) observeEvent(ctx context.Context, data any, errs []gqlerrors.FormattedError) {
	if g.outcomeObs == nil || len(errs) == 0 {
		return
	}
	o := Outcome{ErrorKind: "execution", Partial: true}
	if data == nil {
		o = planErrorOutcome(errs)
	}
	o.Subscription = true
	g.outcomeObs(ctx, o)
}
