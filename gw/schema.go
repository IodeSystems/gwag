package gateway

import (
	"context"
	"fmt"
	"sort"
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
	pol := &policy{hides: map[protoreflect.FullName]bool{}}
	for _, p := range g.pairs {
		for _, t := range p.Hides {
			pol.hides[t] = true
		}
	}

	tb := &typeBuilder{
		policy:  pol,
		objects: map[protoreflect.FullName]*graphql.Object{},
		inputs:  map[protoreflect.FullName]*graphql.InputObject{},
		enums:   map[protoreflect.FullName]*graphql.Enum{},
	}

	chain := g.runtimeChain()
	rootFields := graphql.Fields{}

	// Group pools by namespace, applying the selector filter.
	byNS := map[string][]*pool{}
	for _, p := range g.pools {
		if !filter.matchPool(p.key) {
			continue
		}
		byNS[p.key.namespace] = append(byNS[p.key.namespace], p)
	}

	for ns, pools := range byNS {
		if g.isInternal(ns) {
			continue
		}
		// Sort versions ascending for stable iteration; latest = last.
		sort.Slice(pools, func(i, j int) bool { return pools[i].versionN < pools[j].versionN })
		latest := pools[len(pools)-1]
		latestReason := fmt.Sprintf("%s is current", latest.key.version)

		nsFields := graphql.Fields{}

		// Latest version's RPCs flat under the namespace — what
		// version-agnostic clients see. SchemaIDs for the flat-at-top
		// names use the bare RPC name (no version prefix); the
		// versioned sub-objects below register additional aliases
		// (`<vN>_<rpc>`) so both surfaces resolve through the same
		// dispatcher.
		latestRPCs, err := buildPoolRPCs(g.dispatchers, tb, latest, chain, g.cfg.metrics, g.cfg.backpressure, "")
		if err != nil {
			return nil, err
		}
		for name, f := range latestRPCs {
			nsFields[name] = f
		}

		// Every version (including latest) addressable as a sub-object —
		// unless it has zero unary RPCs, in which case the sub-object
		// would be empty and graphql-go rejects empty Object types.
		// (Subscription-only namespaces hit this; their fields land on
		// the Subscription root instead.)
		for _, p := range pools {
			versionedRPCs, err := buildPoolRPCs(g.dispatchers, tb, p, chain, g.cfg.metrics, g.cfg.backpressure, p.key.version+"_")
			if err != nil {
				return nil, err
			}
			if len(versionedRPCs) == 0 {
				continue
			}
			vName := exportedName(ns) + "_" + exportedName(p.key.version)
			vObj := graphql.NewObject(graphql.ObjectConfig{
				Name:   vName,
				Fields: versionedRPCs,
			})
			subField := &graphql.Field{
				Type: vObj,
				Resolve: func(rp graphql.ResolveParams) (any, error) {
					return struct{}{}, nil
				},
			}
			if p.versionN != latest.versionN {
				subField.DeprecationReason = latestReason
			}
			nsFields[p.key.version] = subField
		}

		// Namespace with only streaming RPCs (e.g. admin_events) has
		// no Query-side surface; skip the top-level field. Subscription
		// fields are still emitted by buildSubscriptionFields below.
		if len(nsFields) == 0 {
			continue
		}

		nsObj := graphql.NewObject(graphql.ObjectConfig{
			Name:   exportedName(ns) + "Namespace",
			Fields: nsFields,
		})
		rootFields[ns] = &graphql.Field{
			Type: nsObj,
			Resolve: func(rp graphql.ResolveParams) (any, error) {
				return struct{}{}, nil
			},
		}
	}

	// Merge OpenAPI fields into the Query and Mutation roots.
	openTB := newOpenAPITypeBuilder()
	openQueries, openMutations, err := g.buildOpenAPIFields(openTB, filter)
	if err != nil {
		return nil, err
	}
	for k, v := range openQueries {
		if _, exists := rootFields[k]; exists {
			return nil, fmt.Errorf("openapi/proto field collision in Query: %s", k)
		}
		rootFields[k] = v
	}

	// Merge downstream-GraphQL ingest fields. Same collision rules.
	gqlQueries, gqlMutations, gqlSubs, err := g.buildGraphQLFields(filter)
	if err != nil {
		return nil, err
	}
	for k, v := range gqlQueries {
		if _, exists := rootFields[k]; exists {
			return nil, fmt.Errorf("graphql ingest field collision in Query: %s", k)
		}
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

	mutationFields := openMutations
	for k, v := range gqlMutations {
		if mutationFields == nil {
			mutationFields = graphql.Fields{}
		}
		if _, exists := mutationFields[k]; exists {
			return nil, fmt.Errorf("graphql ingest field collision in Mutation: %s", k)
		}
		mutationFields[k] = v
	}
	if len(mutationFields) > 0 {
		cfg.Mutation = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Mutation",
			Fields: mutationFields,
		})
	}

	// Subscription root: one flat field per (namespace, server-streaming
	// method) across all pools. Args = request fields + injected hmac
	// (String!) + timestamp (Int!). Subscribe resolver is a no-op stub
	// until the WebSocket transport lands; SDL still surfaces them so
	// codegen pipelines can generate typed subscriptions today.
	subFields, err := g.buildSubscriptionFields(tb, filter)
	if err != nil {
		return nil, err
	}
	for name, f := range gqlSubs {
		if subFields == nil {
			subFields = graphql.Fields{}
		}
		if _, exists := subFields[name]; exists {
			return nil, fmt.Errorf("graphql ingest field collision in Subscription: %s", name)
		}
		subFields[name] = f
	}
	if len(subFields) > 0 {
		cfg.Subscription = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Subscription",
			Fields: subFields,
		})
	}

	schema, err := graphql.NewSchema(cfg)
	if err != nil {
		return nil, fmt.Errorf("graphql.NewSchema: %w", err)
	}
	return &schema, nil
}

// buildSubscriptionFields walks every non-internal pool, finds
// server-streaming RPCs (one request → many responses), and returns
// a graphql.Fields map. Client-streaming and bidi are NOT promoted;
// they're filtered with a warning at registration time (see
// control.go).
//
// Multi-version: sources are grouped by namespace and sorted by
// versionN. The latest version's subscription methods use the flat
// "<ns>_<method>" naming (back-compat with the single-version case).
// Older versions disambiguate via "<ns>_<vN>_<method>" and stamp
// GraphQL @deprecated. Same shape as buildOpenAPIFields /
// buildGraphQLFields — Subscription is flat (graphql-go doesn't
// support nested namespace objects under Subscription), so we
// disambiguate by name rather than by structure.
func (g *Gateway) buildSubscriptionFields(tb *typeBuilder, filter schemaFilter) (graphql.Fields, error) {
	out := graphql.Fields{}

	byNS := map[string][]*pool{}
	for _, p := range g.pools {
		if g.isInternal(p.key.namespace) {
			continue
		}
		if !filter.matchPool(p.key) {
			continue
		}
		byNS[p.key.namespace] = append(byNS[p.key.namespace], p)
	}

	nsNames := make([]string, 0, len(byNS))
	for ns := range byNS {
		nsNames = append(nsNames, ns)
	}
	sort.Strings(nsNames)

	for _, ns := range nsNames {
		pools := byNS[ns]
		sort.Slice(pools, func(i, j int) bool { return pools[i].versionN < pools[j].versionN })
		latest := pools[len(pools)-1]
		latestReason := fmt.Sprintf("%s is current", latest.key.version)

		for _, p := range pools {
			isLatest := p.versionN == latest.versionN
			services := p.file.Services()
			for i := 0; i < services.Len(); i++ {
				sd := services.Get(i)
				methods := sd.Methods()
				for j := 0; j < methods.Len(); j++ {
					md := methods.Get(j)
					if !(md.IsStreamingServer() && !md.IsStreamingClient()) {
						continue
					}
					field, err := g.buildSubscriptionField(tb, p, sd, md)
					if err != nil {
						return nil, err
					}
					var name string
					if isLatest {
						name = ns + "_" + lowerCamel(string(md.Name()))
					} else {
						name = ns + "_" + p.key.version + "_" + lowerCamel(string(md.Name()))
						field.DeprecationReason = latestReason
					}
					if _, exists := out[name]; exists {
						return nil, fmt.Errorf("subscription field name collision: %s", name)
					}
					out[name] = field
				}
			}
		}
	}
	return out, nil
}

func (g *Gateway) buildSubscriptionField(
	tb *typeBuilder,
	p *pool,
	sd protoreflect.ServiceDescriptor,
	md protoreflect.MethodDescriptor,
) (*graphql.Field, error) {
	args, err := tb.argsFromMessage(md.Input())
	if err != nil {
		return nil, err
	}
	// Auto-injected auth args. Surface in SDL so codegen pipelines see
	// them; gateway transport will populate/verify at subscribe time.
	args["hmac"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}
	args["timestamp"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.Int)}
	// Optional key id for token rotation. Clients targeting the legacy
	// single-key Secret leave it null (or omit it). Clients targeting a
	// rotated key from SubscriptionAuthOptions.Secrets pass the kid here
	// — the verifier uses it both to select the secret and as part of
	// the signed payload.
	args["kid"] = &graphql.ArgumentConfig{Type: graphql.String}

	outputType, err := tb.objectFromMessage(md.Output())
	if err != nil {
		return nil, err
	}

	outputDesc := md.Output()
	ns, ver := p.key.namespace, p.key.version
	methodName := string(md.Name())

	return &graphql.Field{
		Type: outputType,
		Args: args,
		Subscribe: func(rp graphql.ResolveParams) (any, error) {
			return g.subscribeNATS(rp.Context, ns, ver, methodName, rp.Args, outputDesc)
		},
		Resolve: func(rp graphql.ResolveParams) (any, error) {
			return rp.Source, nil
		},
	}, nil
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
func buildPoolRPCs(registry *ir.DispatchRegistry, tb *typeBuilder, p *pool, chain Middleware, metrics Metrics, bp BackpressureOptions, flatPrefix string) (graphql.Fields, error) {
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
			field, err := buildPoolMethodField(registry, tb, p, sd, md, chain, metrics, bp, flatPrefix)
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
	tb *typeBuilder,
	p *pool,
	sd protoreflect.ServiceDescriptor,
	md protoreflect.MethodDescriptor,
	chain Middleware,
	metrics Metrics,
	bp BackpressureOptions,
	flatPrefix string,
) (*graphql.Field, error) {
	args, err := tb.argsFromMessage(md.Input())
	if err != nil {
		return nil, err
	}
	outputType, err := tb.objectFromMessage(md.Output())
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

// policy collects schema-rewrite directives extracted from Pairs prior
// to type construction (graphql-go does not let us mutate input fields
// post-hoc, so all hide rules must apply during NewObject).
type policy struct {
	hides map[protoreflect.FullName]bool
}
