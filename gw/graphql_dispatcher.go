package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/printer"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// graphQLForwardInfoKey carries the resolver's ResolveInfo (FieldASTs +
// Operation + VariableValues) into the dispatcher. GraphQL→GraphQL
// forwarding can't reconstruct an equivalent upstream query from
// canonical args alone — it needs the user's selection-set so
// nested subselections forward verbatim. The other ingest paths
// (HTTP/JSON, gRPC) won't have this; the dispatcher returns INTERNAL
// when the key is missing, mirroring the pre-cutover guard.
type graphQLForwardInfoKey struct{}

func withGraphQLForwardInfo(ctx context.Context, info *graphql.ResolveInfo) context.Context {
	return context.WithValue(ctx, graphQLForwardInfoKey{}, info)
}

func graphQLForwardInfoFrom(ctx context.Context) *graphql.ResolveInfo {
	v, _ := ctx.Value(graphQLForwardInfoKey{}).(*graphql.ResolveInfo)
	return v
}

// graphQLDispatcher implements ir.Dispatcher for one downstream
// GraphQL source + one mirrored field. Everything inside the
// pre-cutover forwardingResolver closure (AST rewrite, doc print,
// pickReplica, dispatchGraphQL, response decode) lives here;
// BackpressureMiddleware wraps the outside.
//
// args is unused today — selection-preserving forwarding needs the
// AST, which arrives via withGraphQLForwardInfo. The signature
// stays canonical so an HTTP/JSON ingress can still target a
// graphql-mirror source once the forwarding strategy can synthesize
// a query from canonical args (out of scope for the cutover).
type graphQLDispatcher struct {
	mirror          *graphQLMirror
	remoteFieldName string
	opLabel         string // "query" / "mutation" / "subscription"
	metrics         Metrics
	ns              string
	ver             string
	label           string // "<opLabel> <remoteFieldName>"
}

func newGraphQLDispatcher(m *graphQLMirror, remoteFieldName, opLabel string, metrics Metrics) *graphQLDispatcher {
	return &graphQLDispatcher{
		mirror:          m,
		remoteFieldName: remoteFieldName,
		opLabel:         opLabel,
		metrics:         metrics,
		ns:              m.src.namespace,
		ver:             m.src.version,
		label:           opLabel + " " + remoteFieldName,
	}
}

func (d *graphQLDispatcher) Dispatch(ctx context.Context, _ map[string]any) (any, error) {
	if d.opLabel == "subscription" {
		return d.dispatchSubscribe(ctx)
	}
	return d.dispatchUnary(ctx)
}

func (d *graphQLDispatcher) dispatchUnary(ctx context.Context) (any, error) {
	start := time.Now()
	record := func(err error) error {
		elapsed := time.Since(start)
		d.metrics.RecordDispatch(ctx, d.ns, d.ver, d.label, elapsed, err)
		addDispatchTime(ctx, elapsed)
		return err
	}

	info := graphQLForwardInfoFrom(ctx)
	if info == nil || len(info.FieldASTs) == 0 {
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: no AST for %s", d.remoteFieldName)))
	}
	rewritten := d.mirror.rewriteFieldForRemote(info.FieldASTs[0], d.remoteFieldName)
	opType := ast.OperationTypeQuery
	var varDefs []*ast.VariableDefinition
	if op, ok := info.Operation.(*ast.OperationDefinition); ok && op != nil {
		if op.Operation == ast.OperationTypeMutation {
			opType = ast.OperationTypeMutation
		}
		varDefs = op.VariableDefinitions
	}
	opDef := ast.NewOperationDefinition(&ast.OperationDefinition{
		Operation:           opType,
		VariableDefinitions: varDefs,
		SelectionSet: ast.NewSelectionSet(&ast.SelectionSet{
			Selections: []ast.Selection{rewritten},
		}),
	})
	doc := ast.NewDocument(&ast.Document{Definitions: []ast.Node{opDef}})
	raw := printer.Print(doc)
	printed, ok := raw.(string)
	if !ok {
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: printer returned %T (%v)", raw, raw)))
	}

	src := d.mirror.src
	r := src.pickReplica()
	if r == nil {
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql ingest: no live replicas for %s", d.ns)))
	}
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	resp, err := dispatchGraphQL(ctx, r.httpClient, r.endpoint, printed, info.VariableValues, src.forwardHeaders)
	if err != nil {
		return nil, record(err)
	}
	if len(resp.Errors) > 0 {
		// Surface the first error back to the local client so they
		// see what the remote complained about. Application-level
		// remote errors classify as INTERNAL — there's no portable
		// status code in the GraphQL error envelope.
		return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql remote: %s", resp.Errors[0])))
	}
	var data map[string]any
	if len(resp.Data) > 0 {
		if err := jsonUnmarshalLoose(resp.Data, &data); err != nil {
			return nil, record(Reject(CodeInternal, fmt.Sprintf("graphql decode data: %s", err.Error())))
		}
	}
	record(nil)
	return data[d.remoteFieldName], nil
}

// dispatchSubscribe opens a multiplexed subscription against the
// upstream and returns a chan any of pre-plucked frames. The
// renderer's Subscribe path treats the dispatcher result as the
// subscribe channel; its Resolve closure surfaces rp.Source directly,
// so this goroutine plucks `frame.Result[remoteFieldName]` before
// emitting (matching the pre-cutover subscribingResolver shape where
// the local Resolve picked the field out of the data envelope).
//
// Backpressure middleware is intentionally not wrapped around the
// subscribe path — src.sem is per-source unary capacity, not stream
// lifetime. The pre-cutover subscribingResolver bypassed
// BackpressureMiddleware for the same reason; the multiplexer broker
// is the rate-control story for streams.
func (d *graphQLDispatcher) dispatchSubscribe(ctx context.Context) (any, error) {
	info := graphQLForwardInfoFrom(ctx)
	if info == nil || len(info.FieldASTs) == 0 {
		return nil, fmt.Errorf("graphql ingest: no AST for subscription %s", d.remoteFieldName)
	}
	rewritten := d.mirror.rewriteFieldForRemote(info.FieldASTs[0], d.remoteFieldName)
	var varDefs []*ast.VariableDefinition
	if op, ok := info.Operation.(*ast.OperationDefinition); ok && op != nil {
		varDefs = op.VariableDefinitions
	}
	opDef := ast.NewOperationDefinition(&ast.OperationDefinition{
		Operation:           ast.OperationTypeSubscription,
		VariableDefinitions: varDefs,
		SelectionSet: ast.NewSelectionSet(&ast.SelectionSet{
			Selections: []ast.Selection{rewritten},
		}),
	})
	doc := ast.NewDocument(&ast.Document{Definitions: []ast.Node{opDef}})
	raw := printer.Print(doc)
	printed, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("graphql ingest: printer returned %T (%v)", raw, raw)
	}

	src := d.mirror.src
	r := src.pickReplica()
	if r == nil {
		return nil, fmt.Errorf("graphql ingest: no live replicas for %s", src.namespace)
	}
	broker := src.getSubBroker()
	upstream, release, err := broker.acquire(ctx, r.endpoint, printed, info.VariableValues, src.forwardHeaders)
	if err != nil {
		return nil, err
	}
	out := make(chan any, 8)
	remote := d.remoteFieldName
	go func() {
		defer close(out)
		defer release()
		for {
			select {
			case f, ok := <-upstream:
				if !ok {
					return
				}
				if f == nil {
					continue
				}
				if len(f.Errors) > 0 {
					select {
					case out <- fmt.Errorf("graphql remote: %s", f.Errors[0]):
					case <-ctx.Done():
						return
					}
					if f.Done {
						return
					}
					continue
				}
				if f.Result != nil {
					select {
					case out <- f.Result[remote]:
					case <-ctx.Done():
						return
					}
				}
				if f.Done {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// graphQLBackpressureConfig bundles a graphql source's per-dispatch
// knobs for BackpressureMiddleware. Sibling of pool / openAPI
// equivalents.
func graphQLBackpressureConfig(src *graphQLSource, label string, metrics Metrics, bp BackpressureOptions) BackpressureConfig {
	return BackpressureConfig{
		Sem:         src.sem,
		Queueing:    &src.queueing,
		MaxWaitTime: bp.MaxWaitTime,
		Metrics:     metrics,
		Namespace:   src.namespace,
		Version:     src.version,
		Label:       label,
		Kind:        "unary",
	}
}

var _ ir.Dispatcher = (*graphQLDispatcher)(nil)
