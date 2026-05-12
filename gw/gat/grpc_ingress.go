package gat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/gw/ir"
)

// HandleMux is the minimal mux surface RegisterGRPC writes to. Stdlib
// *http.ServeMux satisfies it; chi/gorilla/etc. do too.
type HandleMux interface {
	Handle(pattern string, handler http.Handler)
}

// RegisterGRPC mounts a connect-go handler per captured operation on
// mux, under prefix. Each operation becomes available at the
// canonical gRPC/Connect path:
//
//	{prefix}/{Service.FullName}/{MethodName}
//
// Wire-protocol coverage is what connect-go gives: clients can use
// grpc-go (HTTP/2 native), connect-go (Connect protocol), or
// grpc-web (browser-friendly) — all hit the same handlers.
//
// Dispatch round-trip: incoming proto (dynamic message) →
// protojson → map[string]any → bindInput → captured huma handler →
// extracted Body → JSON → protojson → outgoing proto.
//
// Must be called AFTER RegisterHuma — RegisterGRPC reads the
// FileDescriptorSet projection of g.services, which only exists
// once the schema is built.
func RegisterGRPC(mux HandleMux, g *Gateway, prefix string) error {
	if !g.built {
		return fmt.Errorf("gat: RegisterHuma must be called before RegisterGRPC")
	}
	fds, err := ir.RenderProtoFiles(g.services)
	if err != nil {
		return fmt.Errorf("gat: render proto files: %w", err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return fmt.Errorf("gat: build files registry: %w", err)
	}

	capturedByName := make(map[string]*capturedOp, len(g.captured))
	for _, c := range g.captured {
		capturedByName[c.op.OperationID] = c
	}

	prefix = strings.TrimRight(prefix, "/")

	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			sd := fd.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				md := sd.Methods().Get(j)
				name := string(md.Name())
				cap, ok := capturedByName[name]
				if !ok {
					continue
				}
				procedure := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
				handler := connect.NewUnaryHandler(
					procedure,
					connectUnary(cap, md),
					connect.WithSchema(md),
					connect.WithRequestInitializer(newDynamicInitializer()),
				)
				mux.Handle(prefix+procedure, handler)
			}
		}
		return true
	})

	return nil
}

// newDynamicInitializer returns a connect.WithRequestInitializer that
// allocates a dynamicpb.Message of the right MethodDescriptor input
// (or output, on the client side) shape.
func newDynamicInitializer() func(connect.Spec, any) error {
	return func(spec connect.Spec, msg any) error {
		dyn, ok := msg.(*dynamicpb.Message)
		if !ok {
			return nil
		}
		md, ok := spec.Schema.(protoreflect.MethodDescriptor)
		if !ok {
			return fmt.Errorf("gat: spec.Schema is %T, want protoreflect.MethodDescriptor", spec.Schema)
		}
		if spec.IsClient {
			*dyn = *dynamicpb.NewMessage(md.Output())
		} else {
			*dyn = *dynamicpb.NewMessage(md.Input())
		}
		return nil
	}
}

// connectUnary builds the typed unary handler that bridges
// dynamicpb messages to the captured huma handler.
func connectUnary(cap *capturedOp, md protoreflect.MethodDescriptor) func(context.Context, *connect.Request[dynamicpb.Message]) (*connect.Response[dynamicpb.Message], error) {
	return func(ctx context.Context, req *connect.Request[dynamicpb.Message]) (*connect.Response[dynamicpb.Message], error) {
		args, err := dynamicToArgs(req.Msg)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		inPtr := reflect.New(cap.inputType)
		in := inPtr.Elem()
		bodyArg := ""
		if cap.irOp != nil {
			for _, a := range cap.irOp.Args {
				if strings.EqualFold(a.OpenAPILocation, "body") {
					bodyArg = a.Name
					break
				}
			}
			if err := bindInput(in, cap.irOp, args, bodyArg); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("bind %s: %w", cap.op.OperationID, err))
			}
		} else {
			// Best-effort JSON-into-input for ops never ingested.
			raw, _ := json.Marshal(args)
			_ = json.Unmarshal(raw, inPtr.Interface())
		}

		out, err := cap.invoke(ctx, inPtr.Interface())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		body := extractBody(out)

		respMsg := dynamicpb.NewMessage(md.Output())
		if err := jsonToDynamic(body, respMsg); err != nil {
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("encode response: %w", err))
		}
		return connect.NewResponse(respMsg), nil
	}
}

// dynamicToArgs marshals the dynamic proto to canonical JSON, then
// decodes that JSON into a map[string]any keyed by the field's JSON
// name. This matches how IR Args land on the wire (one Arg per
// top-level field).
func dynamicToArgs(msg *dynamicpb.Message) (map[string]any, error) {
	if msg == nil {
		return map[string]any{}, nil
	}
	b, err := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: false,
	}.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("protojson encode: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// jsonToDynamic encodes body as JSON and decodes that into the
// dynamic proto message. The round-trip relies on body being JSON-
// representable, which it always is for huma outputs.
func jsonToDynamic(body any, msg *dynamicpb.Message) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(b, msg); err != nil {
		return fmt.Errorf("protojson decode: %w", err)
	}
	return nil
}
