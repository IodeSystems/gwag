package gateway

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/graphql-go/graphql"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// assembleLocked walks every pool and builds a single graphql.Schema.
// One Query field per non-internal namespace; under each namespace, the
// latest version's RPCs surface flat AND every version is addressable
// via vN sub-objects (non-latest sub-objects marked @deprecated). The
// dispatch closure picks a replica from the pool at invocation time.
// Caller holds g.mu. Atomically replaces g.schema on success.
func (g *Gateway) assembleLocked() error {
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

	// Group pools by namespace.
	byNS := map[string][]*pool{}
	for _, p := range g.pools {
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
		// version-agnostic clients see.
		latestRPCs, err := buildPoolRPCs(tb, latest, chain, g.cfg.metrics, g.cfg.backpressure)
		if err != nil {
			return err
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
			versionedRPCs, err := buildPoolRPCs(tb, p, chain, g.cfg.metrics, g.cfg.backpressure)
			if err != nil {
				return err
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
	openQueries, openMutations, err := g.buildOpenAPIFields(openTB)
	if err != nil {
		return err
	}
	for k, v := range openQueries {
		if _, exists := rootFields[k]; exists {
			return fmt.Errorf("openapi/proto field collision in Query: %s", k)
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

	if len(openMutations) > 0 {
		cfg.Mutation = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Mutation",
			Fields: openMutations,
		})
	}

	// Subscription root: one flat field per (namespace, server-streaming
	// method) across all pools. Args = request fields + injected hmac
	// (String!) + timestamp (Int!). Subscribe resolver is a no-op stub
	// until the WebSocket transport lands; SDL still surfaces them so
	// codegen pipelines can generate typed subscriptions today.
	subFields, err := g.buildSubscriptionFields(tb)
	if err != nil {
		return err
	}
	if len(subFields) > 0 {
		cfg.Subscription = graphql.NewObject(graphql.ObjectConfig{
			Name:   "Subscription",
			Fields: subFields,
		})
	}

	schema, err := graphql.NewSchema(cfg)
	if err != nil {
		return fmt.Errorf("graphql.NewSchema: %w", err)
	}
	g.schema.Store(&schema)
	return nil
}

// buildSubscriptionFields walks every non-internal pool, finds
// server-streaming RPCs (one request → many responses), and returns
// a graphql.Fields map keyed by "<namespace>_<lowerCamel(method)>".
// Client-streaming and bidi are NOT promoted; they're filtered with a
// warning at registration time (see control.go).
func (g *Gateway) buildSubscriptionFields(tb *typeBuilder) (graphql.Fields, error) {
	out := graphql.Fields{}
	for _, p := range g.pools {
		if g.isInternal(p.key.namespace) {
			continue
		}
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
				name := p.key.namespace + "_" + lowerCamel(string(md.Name()))
				if _, exists := out[name]; exists {
					return nil, fmt.Errorf("subscription field name collision: %s", name)
				}
				out[name] = field
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
// the pool's proto. The dispatch closure picks a replica via
// pool.pickReplica at invocation time.
func buildPoolRPCs(tb *typeBuilder, p *pool, chain Middleware, metrics Metrics, bp BackpressureOptions) (graphql.Fields, error) {
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
			field, err := buildPoolMethodField(tb, p, sd, md, chain, metrics, bp)
			if err != nil {
				return nil, err
			}
			out[lowerCamel(string(md.Name()))] = field
		}
	}
	return out, nil
}

func buildPoolMethodField(
	tb *typeBuilder,
	p *pool,
	sd protoreflect.ServiceDescriptor,
	md protoreflect.MethodDescriptor,
	chain Middleware,
	metrics Metrics,
	bp BackpressureOptions,
) (*graphql.Field, error) {
	inputDesc := md.Input()
	outputDesc := md.Output()

	args, err := tb.argsFromMessage(inputDesc)
	if err != nil {
		return nil, err
	}
	outputType, err := tb.objectFromMessage(outputDesc)
	if err != nil {
		return nil, err
	}

	method := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
	ns, ver := p.key.namespace, p.key.version

	dispatch := Handler(func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		start := time.Now()

		// Acquire a slot. Fast path: pool.sem nil (unbounded) or
		// has immediate capacity. Slow path: queue, observe dwell,
		// time out per MaxWaitTime.
		if p.sem != nil {
			waitStart := time.Now()
			select {
			case p.sem <- struct{}{}:
				metrics.RecordDwell(ns, ver, method, "unary", time.Since(waitStart))
			default:
				depth := int(p.queueing.Add(1))
				metrics.SetQueueDepth(ns, ver, "unary", depth)
				dwell, err := waitForSlot(ctx, p.sem, bp.MaxWaitTime)
				now := int(p.queueing.Add(-1))
				metrics.SetQueueDepth(ns, ver, "unary", now)
				metrics.RecordDwell(ns, ver, method, "unary", dwell)
				if err != nil {
					reason := "wait_timeout"
					rejErr := Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", ns, ver, err.Error()))
					metrics.RecordBackoff(ns, ver, method, "unary", reason)
					metrics.RecordDispatch(ns, ver, method, time.Since(start), rejErr)
					return nil, rejErr
				}
			}
			defer func() { <-p.sem }()
		}

		r := p.pickReplica()
		if r == nil {
			err := fmt.Errorf("gateway: no live replicas for %s/%s", ns, ver)
			metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
			return nil, err
		}
		r.inflight.Add(1)
		defer r.inflight.Add(-1)
		resp := dynamicpb.NewMessage(outputDesc)
		err := r.conn.Invoke(ctx, method, req, resp)
		metrics.RecordDispatch(ns, ver, method, time.Since(start), err)
		if err != nil {
			return nil, err
		}
		return resp, nil
	})
	wrapped := chain(dispatch)

	return &graphql.Field{
		Type: outputType,
		Args: args,
		Resolve: func(rp graphql.ResolveParams) (any, error) {
			req := dynamicpb.NewMessage(inputDesc)
			if err := argsToMessage(rp.Args, req); err != nil {
				return nil, err
			}
			resp, err := wrapped(rp.Context, req)
			if err != nil {
				return nil, err
			}
			return messageToMap(resp.(*dynamicpb.Message)), nil
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
