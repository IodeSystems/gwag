package gateway

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// assembleLocked walks every registered service and builds a single
// graphql.Schema with one Query field per non-internal namespace. Each
// namespace exposes one field per RPC method; the resolver bridges into
// the runtime middleware chain and dispatches via dynamicpb. Caller
// holds g.mu. Atomically replaces g.schema on success.
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

	rootFields := graphql.Fields{}

	for _, svc := range g.services {
		if svc.internal {
			continue
		}
		nsFields, err := buildNamespaceFields(tb, svc, g.runtimeChain())
		if err != nil {
			return err
		}
		if len(nsFields) == 0 {
			continue
		}
		nsName := exportedName(svc.namespace) + "Namespace"
		nsObj := graphql.NewObject(graphql.ObjectConfig{
			Name:   nsName,
			Fields: nsFields,
		})
		rootFields[svc.namespace] = &graphql.Field{
			Type: nsObj,
			Resolve: func(p graphql.ResolveParams) (any, error) {
				return struct{}{}, nil
			},
		}
	}

	// graphql-go requires Query to have at least one field. When no
	// public services are registered (boot pre-control-plane, or after
	// the last service deregistered), expose an inert _status field so
	// the schema is valid and clients see "ok, nothing routed yet".
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

// policy collects schema-rewrite directives extracted from Pairs prior
// to type construction (graphql-go does not let us mutate input fields
// post-hoc, so all hide rules must apply during NewObject).
type policy struct {
	hides map[protoreflect.FullName]bool
}

// buildNamespaceFields produces one graphql.Field per RPC method in the
// service's file descriptor. Each field's Resolve closure marshals
// graphql args to a dynamicpb input message, runs the runtime
// middleware chain, then dispatches via grpc.ClientConn.Invoke.
func buildNamespaceFields(
	tb *typeBuilder,
	svc *registeredService,
	chain Middleware,
) (graphql.Fields, error) {
	out := graphql.Fields{}
	services := svc.file.Services()
	for i := 0; i < services.Len(); i++ {
		sd := services.Get(i)
		methods := sd.Methods()
		for j := 0; j < methods.Len(); j++ {
			md := methods.Get(j)
			if md.IsStreamingClient() || md.IsStreamingServer() {
				continue
			}
			field, err := buildMethodField(tb, svc.conn, sd, md, chain)
			if err != nil {
				return nil, err
			}
			out[lowerCamel(string(md.Name()))] = field
		}
	}
	return out, nil
}

func buildMethodField(
	tb *typeBuilder,
	conn grpc.ClientConnInterface,
	sd protoreflect.ServiceDescriptor,
	md protoreflect.MethodDescriptor,
	chain Middleware,
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

	dispatch := Handler(func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		resp := dynamicpb.NewMessage(outputDesc)
		if err := conn.Invoke(ctx, method, req, resp); err != nil {
			return nil, err
		}
		return resp, nil
	})
	wrapped := chain(dispatch)

	return &graphql.Field{
		Type: outputType,
		Args: args,
		Resolve: func(p graphql.ResolveParams) (any, error) {
			req := dynamicpb.NewMessage(inputDesc)
			if err := argsToMessage(p.Args, req); err != nil {
				return nil, err
			}
			resp, err := wrapped(p.Context, req)
			if err != nil {
				return nil, err
			}
			return messageToMap(resp.(*dynamicpb.Message)), nil
		},
	}, nil
}

// runtimeChain returns the composed Middleware for runtime hooks. Pairs
// without a Runtime half are skipped.
func (g *Gateway) runtimeChain() Middleware {
	return func(next Handler) Handler {
		h := next
		// apply in reverse so first Use() is outermost
		for i := len(g.pairs) - 1; i >= 0; i-- {
			if g.pairs[i].Runtime != nil {
				h = g.pairs[i].Runtime(h)
			}
		}
		return h
	}
}
