package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/graphql-go/graphql"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// assembleLocked builds the gateway's canonical (unfiltered) GraphQL
// schema and atomically swaps it into g.schema. Caller holds g.mu.
//
// The DispatchRegistry is rebuilt fresh: every schema rebuild
// re-creates dispatchers (because the user runtime middleware chain
// can change between rebuilds), so any stale entries from a prior
// rebuild must go. Filtered schema builds (via /api/schema/graphql
// `?service=`) reuse this same registry — the per-call schemas
// produced by buildSchemaLocked share dispatcher state across all
// callers since the dispatcher's runtime behavior is independent of
// which subset is rendered.
func (g *Gateway) assembleLocked() error {
	g.dispatchers = ir.NewDispatchRegistry()
	schema, err := g.buildSchemaLocked(schemaFilter{})
	if err != nil {
		return err
	}
	g.schema.Store(schema)
	return nil
}

// buildSchemaLocked walks the registered pools / OpenAPI sources /
// downstream-GraphQL sources matching `filter` and produces a fresh
// graphql.Schema. An empty filter (zero value) matches everything —
// that's what assembleLocked uses. /schema/graphql passes a populated
// filter so codegen consumers can fetch a single namespace's slice
// without rebuilding the gateway-wide schema.
//
// Caller holds g.mu. Returns the built schema; does NOT store it.
func (g *Gateway) buildSchemaLocked(filter schemaFilter) (*graphql.Schema, error) {
	hidesSet := g.hidesSet()

	// Share one *IRTypeBuilder across every proto pool so cyclic
	// message refs collapse to a single *graphql.Object across Query,
	// Mutation, and Subscription roots — and so v1 + v2 of the same
	// proto package don't trip graphql-go's duplicate-named-type
	// rejection (proto FullNames are globally unique, so a single
	// merged Types map is collision-free).
	protoTB := newProtoIRTypeBuilder(g.pools, hidesSet)

	rootFields := graphql.Fields{}

	// Proto + OpenAPI + GraphQL ingest all go through
	// RenderGraphQLRuntimeFields in a single call so the multi-version
	// fold sees every service in a namespace together (avoiding
	// separate render calls that would each emit a `<ns>` root field
	// and collide on merge). Per-kind dispatcher registration runs
	// first, then service distillation, then the render.
	g.registerProtoDispatchersLocked(filter)
	protoSvcs, err := g.protoServicesAsIRLocked(filter)
	if err != nil {
		return nil, err
	}
	openSvcs, err := g.openAPIServicesAsIRLocked(filter)
	if err != nil {
		return nil, err
	}
	if err := g.registerOpenAPIDispatchersLocked(openSvcs); err != nil {
		return nil, err
	}
	gqlSvcs, err := g.graphQLServicesAsIRLocked(filter)
	if err != nil {
		return nil, err
	}
	if err := g.registerGraphQLDispatchersLocked(gqlSvcs); err != nil {
		return nil, err
	}
	long, jsonScalar := openAPISharedScalars()
	allSvcs := make([]*ir.Service, 0, len(protoSvcs)+len(openSvcs)+len(gqlSvcs))
	allSvcs = append(allSvcs, protoSvcs...)
	allSvcs = append(allSvcs, openSvcs...)
	allSvcs = append(allSvcs, gqlSvcs...)
	queries, mutations, runtimeSubs, err := RenderGraphQLRuntimeFields(allSvcs, g.dispatchers, RuntimeOptions{
		SharedProtoBuilder: protoTB,
		LongType:           long,
		JSONType:           jsonScalar,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime render: %w", err)
	}
	for k, v := range queries {
		rootFields[k] = v
	}

	if len(rootFields) == 0 {
		rootFields["_status"] = &graphql.Field{
			Type: graphql.String,
			Resolve: func(p graphql.ResolveParams) (any, error) {
				return "no services registered", nil
			},
		}
	}

	queryObj := graphql.NewObject(graphql.ObjectConfig{
		Name:   "Query",
		Fields: rootFields,
	})

	cfg := graphql.SchemaConfig{Query: queryObj}

	mutationFields := mutations
	if len(mutationFields) > 0 {
		cfg.Mutation = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Mutation",
			Fields: mutationFields,
		})
	}

	// Subscriptions: every kind (proto server-streaming, graphql-ingest
	// subscriptions) renders through RenderGraphQLRuntimeFields above.
	// OpenAPI has no subscription concept.
	if len(runtimeSubs) > 0 {
		cfg.Subscription = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Subscription",
			Fields: runtimeSubs,
		})
	}

	schema, err := graphql.NewSchema(cfg)
	if err != nil {
		return nil, fmt.Errorf("graphql.NewSchema: %w", err)
	}
	return &schema, nil
}

// buildPoolRPCs returns one graphql.Field per RPC method declared in
// the pool's proto. Each field's Resolve closure looks up its
// dispatcher by SchemaID at call time — the dispatcher itself is
// registered in `registry` here, keyed by
// `<namespace>/<version>/<flatPrefix><lowerCamelMethodName>`.
//
// flatPrefix is empty for the namespace-flat alias and `<version>_`
// for the versioned sub-object alias. Both aliases register the
// same dispatcher under different keys so a query selecting
// `greeter.hello` and `greeter.v1.hello` both resolve.
func buildPoolRPCs(registry *ir.DispatchRegistry, tb *IRTypeBuilder, hides map[string]bool, p *pool, chain Middleware, metrics Metrics, bp BackpressureOptions, flatPrefix string) (graphql.Fields, error) {
	out := graphql.Fields{}
	services := p.file.Services()
	for i := 0; i < services.Len(); i++ {
		sd := services.Get(i)
		methods := sd.Methods()
		for j := 0; j < methods.Len(); j++ {
			md := methods.Get(j)
			if md.IsStreamingClient() || md.IsStreamingServer() {
				continue
			}
			field, err := buildPoolMethodField(registry, tb, hides, p, sd, md, chain, metrics, bp, flatPrefix)
			if err != nil {
				return nil, err
			}
			out[lowerCamel(string(md.Name()))] = field
		}
	}
	return out, nil
}

func buildPoolMethodField(
	registry *ir.DispatchRegistry,
	tb *IRTypeBuilder,
	hides map[string]bool,
	p *pool,
	sd protoreflect.ServiceDescriptor,
	md protoreflect.MethodDescriptor,
	chain Middleware,
	metrics Metrics,
	bp BackpressureOptions,
	flatPrefix string,
) (*graphql.Field, error) {
	args, err := protoArgsFromMessage(tb, md.Input(), hides)
	if err != nil {
		return nil, err
	}
	outputType, err := protoOutputObject(tb, md.Output())
	if err != nil {
		return nil, err
	}

	label := methodLabel(sd, md)
	core := newProtoDispatcher(p, sd, md, chain, metrics)
	dispatcher := BackpressureMiddleware(poolBackpressureConfig(p, label, metrics, bp))(core)
	sid := ir.MakeSchemaID(p.key.namespace, p.key.version, flatPrefix+lowerCamel(string(md.Name())))
	registry.Set(sid, dispatcher)

	return &graphql.Field{
		Type: outputType,
		Args: args,
		Resolve: func(rp graphql.ResolveParams) (any, error) {
			d := registry.Get(sid)
			if d == nil {
				return nil, Reject(CodeInternal, fmt.Sprintf("gateway: no dispatcher for %s", sid))
			}
			return d.Dispatch(rp.Context, rp.Args)
		},
	}, nil
}

// waitForSlot blocks until sem has capacity or the per-dispatch
// MaxWaitTime budget expires. Returns the dwell time and an error
// when the wait times out (or the request context cancels). Used for
// both unary in-flight slots and stream slots.
func waitForSlot(ctx context.Context, sem chan struct{}, maxWait time.Duration) (time.Duration, error) {
	start := time.Now()
	if maxWait <= 0 {
		select {
		case sem <- struct{}{}:
			return time.Since(start), nil
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		}
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
		return time.Since(start), nil
	case <-timer.C:
		return time.Since(start), fmt.Errorf("could not acquire slot in %s", maxWait)
	case <-ctx.Done():
		return time.Since(start), ctx.Err()
	}
}

// runtimeChain returns the composed Middleware for runtime hooks. Pairs
// without a Runtime half are skipped.
func (g *Gateway) runtimeChain() Middleware {
	return func(next Handler) Handler {
		h := next
		for i := len(g.pairs) - 1; i >= 0; i-- {
			if g.pairs[i].Runtime != nil {
				h = g.pairs[i].Runtime(h)
			}
		}
		return h
	}
}

