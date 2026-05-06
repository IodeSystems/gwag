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
		if g.internal[ns] {
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

		// Every version (including latest) addressable as a sub-object.
		for _, p := range pools {
			versionedRPCs, err := buildPoolRPCs(tb, p, chain, g.cfg.metrics, g.cfg.backpressure)
			if err != nil {
				return err
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

	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: queryObj})
	if err != nil {
		return fmt.Errorf("graphql.NewSchema: %w", err)
	}
	g.schema.Store(&schema)
	return nil
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
				metrics.RecordDwell(ns, ver, method, time.Since(waitStart))
			default:
				depth := int(p.queueing.Add(1))
				metrics.SetQueueDepth(ns, ver, depth)
				dwell, err := waitForSlot(ctx, p, bp.MaxWaitTime)
				now := int(p.queueing.Add(-1))
				metrics.SetQueueDepth(ns, ver, now)
				metrics.RecordDwell(ns, ver, method, dwell)
				if err != nil {
					reason := "wait_timeout"
					rejErr := Reject(CodeResourceExhausted, fmt.Sprintf("%s/%s: %s", ns, ver, err.Error()))
					metrics.RecordBackoff(ns, ver, method, reason)
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

// waitForSlot blocks until p.sem has capacity or the per-dispatch
// MaxWaitTime budget expires. Returns the dwell time and an error
// when the wait times out (or the request context cancels).
func waitForSlot(ctx context.Context, p *pool, maxWait time.Duration) (time.Duration, error) {
	start := time.Now()
	if maxWait <= 0 {
		// No wait timeout — block on context only.
		select {
		case p.sem <- struct{}{}:
			return time.Since(start), nil
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		}
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case p.sem <- struct{}{}:
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
