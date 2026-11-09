package gat

// gRPC ingress for operations that are neither captured huma
// operations nor huma streams: OpenAPI operations of a BYO IR service
// (ServiceRegistration), forwarded to an HTTP upstream by a
// Dispatcher. They sit in the GraphQL schema and the rendered proto,
// so RegisterGRPC serves them too.
//
// The request message becomes the dispatcher's args map
// (messageToArgs, which also unboxes nested lists and maps); the
// result goes back through jsonToDynamic (which boxes them again).
// A list or scalar result lands under the synthesized response
// message's "value".

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// byoOps maps "<proto package>.<Service>/<method>" to the BYO OpenAPI
// operation behind it. Operations of huma services, and operations
// without a dispatcher, are left out.
func (g *Gateway) byoOps() map[string]*ir.Operation {
	owned := map[*ir.Operation]bool{}
	for _, c := range g.captured {
		owned[c.irOp] = true
	}
	for _, s := range g.subs {
		owned[s.irOp] = true
	}
	out := map[string]*ir.Operation{}
	for _, svc := range g.services {
		if svc.OriginKind != ir.KindOpenAPI {
			continue
		}
		for _, op := range svc.FlatOperations() {
			if owned[op] || op.StreamingClient || g.registry.Get(op.SchemaID) == nil {
				continue
			}
			out[protoProcKey(svc, op)] = op
		}
	}
	return out
}

// protoProcKey is "<proto package>.<Service>/<method>" for op of an
// OpenAPI-origin svc, as ir.RenderProtoFiles names it: package
// "<namespace>.<version>", service "Service" unless named.
func protoProcKey(svc *ir.Service, op *ir.Operation) string {
	pkg := svc.Namespace
	if svc.Version != "" {
		pkg += "." + svc.Version
	}
	name := svc.ServiceName
	if name == "" {
		name = "Service"
	}
	return pkg + "." + name + "/" + op.Name
}

// byoResponse is the dispatcher's result as the output message.
func byoResponse(md protoreflect.MessageDescriptor, result any) (*dynamicpb.Message, error) {
	msg := dynamicpb.NewMessage(md)
	if result == nil {
		return msg, nil
	}
	if _, isObj := result.(map[string]any); !isObj && isSynthResponseWrapper(md) {
		result = map[string]any{"value": result}
	}
	if err := jsonToDynamic(result, msg); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode response: %w", err))
	}
	return msg, nil
}

// isSynthResponseWrapper reports whether md has the shape
// ir.synthResponseMessage emits: exactly one field, "value".
func isSynthResponseWrapper(md protoreflect.MessageDescriptor) bool {
	return md.Fields().Len() == 1 && md.Fields().Get(0).Name() == "value"
}

// byoPreCall runs the CallHook, if any.
func byoPreCall(g *Gateway, ctx context.Context, req connect.AnyRequest, call GRPCCall) (context.Context, func(context.Context), error) {
	if g.preCall == nil {
		return ctx, nil, nil
	}
	ctx, done, err := g.preCall(withHeaderMetadata(ctx, req.Header()), call)
	if err != nil {
		if done != nil {
			done(ctx)
		}
		return ctx, nil, connectError(err)
	}
	return ctx, done, nil
}

func byoUnary(g *Gateway, op *ir.Operation, md protoreflect.MethodDescriptor, procedure string) func(context.Context, *connect.Request[wireRequest]) (*connect.Response[wireResult], error) {
	call := GRPCCall{Procedure: procedure}
	return func(ctx context.Context, req *connect.Request[wireRequest]) (*connect.Response[wireResult], error) {
		ctx, done, err := byoPreCall(g, ctx, req, call)
		if err != nil {
			return nil, err
		}
		if done != nil {
			defer done(ctx)
		}
		ctx, _ = newScope(ctx, connectInboundRequest(req), false)
		result, err := g.registry.Get(op.SchemaID).Dispatch(ctx, messageToArgs(req.Msg.message()))
		if err != nil {
			return nil, handlerErrorToConnect(err)
		}
		msg, err := byoResponse(md.Output(), result)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(newBuiltResult(md.Output(), msg)), nil
	}
}

func byoStream(g *Gateway, op *ir.Operation, md protoreflect.MethodDescriptor, procedure string) func(context.Context, *connect.Request[wireRequest], *connect.ServerStream[dynamicpb.Message]) error {
	call := GRPCCall{Procedure: procedure, Streaming: true}
	return func(ctx context.Context, req *connect.Request[wireRequest], stream *connect.ServerStream[dynamicpb.Message]) error {
		ctx, done, err := byoPreCall(g, ctx, req, call)
		if err != nil {
			return err
		}
		if done != nil {
			defer done(ctx)
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		ctx, _ = newScope(ctx, connectInboundRequest(req), false)
		result, err := g.registry.Get(op.SchemaID).Dispatch(ctx, messageToArgs(req.Msg.message()))
		if err != nil {
			return handlerErrorToConnect(err)
		}
		ch, ok := result.(chan interface{})
		if !ok {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: dispatcher is not a stream", op.Name))
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case item, open := <-ch:
				if !open {
					return nil
				}
				if e, isErr := item.(error); isErr {
					return handlerErrorToConnect(e)
				}
				msg, err := byoResponse(md.Output(), item)
				if err != nil {
					return err
				}
				if err := stream.Send(msg); err != nil {
					return err
				}
			}
		}
	}
}
