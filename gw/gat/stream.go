package gat

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"connectrpc.com/connect"
	"github.com/IodeSystems/graphql-go/v2/gqlerrors"
	"github.com/danielgtaylor/huma/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// streamOp is one streaming operation, from RegisterSSE or Subscribe.
// Every streaming surface (GraphQL subscription, gRPC server stream)
// drives the same run func, so they deliver the same events from the
// same handler behind the same middleware chain.
type streamOp struct {
	op        huma.Operation // OperationID + the Path it was registered under
	inputType reflect.Type

	// events lists the declared event types (RegisterSSE's eventTypeMap,
	// sorted by name). Empty for Subscribe, whose output type comes from
	// ingest. Two or more make the output a union.
	events []streamEvent

	// run executes one stream: the middleware chain, then the source.
	// It calls started once nothing before the first event can fail,
	// then emit per event; emit returns an error once the consumer is
	// gone. An error returned before started is a rejection or setup
	// failure and is reported to the client.
	run func(ctx context.Context, in any, started func(), emit func(any) error) error

	irOp  *ir.Operation
	union string // IR union type name; "" unless len(events) > 1
}

// streamEvent is one declared event: its SSE event name, Go type, and
// the component schema name huma registered it under.
type streamEvent struct {
	name   string
	goType reflect.Type
	schema string
}

// eventFor returns the declared event matching v's type.
func (s *streamOp) eventFor(v any) (streamEvent, error) {
	t := derefType(reflect.TypeOf(v))
	for _, e := range s.events {
		if e.goType == t {
			return e, nil
		}
	}
	return streamEvent{}, fmt.Errorf("gat: %s: event type %v is not declared", s.op.OperationID, t)
}

// bindStreams marks each streaming op's ingested operation as a
// subscription, sets its output for declared events, and wires the
// GraphQL dispatcher. Called from ingestHuma.
func (g *Gateway) bindStreams(svc *ir.Service) error {
	for _, s := range g.subs {
		for _, op := range svc.FlatOperations() {
			if op.Name == s.op.OperationID && op.HTTPPath == s.op.Path {
				s.irOp = op
				break
			}
		}
		if s.irOp == nil {
			return fmt.Errorf("gat: stream %s missing from ingested schema", s.op.OperationID)
		}
		op := s.irOp
		op.Kind = ir.OpSubscription
		// Same provenance marker ingestHuma puts on Register ops.
		op.Annotations = append(op.Annotations, ir.Annotation{
			Name: "derived",
			Args: []ir.AnnotationArg{{Name: "operationId", Kind: ir.AnnString, Value: op.Name}},
		})
		switch len(s.events) {
		case 0:
			// Subscribe: output already ingested from the side op.
		case 1:
			op.Output = &ir.TypeRef{Named: s.events[0].schema}
			op.OutputRequired = true
		default:
			name := exportedName(s.op.OperationID) + "Event"
			if _, exists := svc.Types[name]; exists {
				return fmt.Errorf("gat: stream %s: union type name %s already taken", s.op.OperationID, name)
			}
			variants := make([]string, len(s.events))
			for i, e := range s.events {
				if _, ok := svc.Types[e.schema]; !ok {
					return fmt.Errorf("gat: stream %s: event type %s missing from ingested schema", s.op.OperationID, e.schema)
				}
				variants[i] = e.schema
			}
			svc.Types[name] = &ir.Type{
				Name:        name,
				TypeKind:    ir.TypeUnion,
				Description: "Events of " + s.op.OperationID + ".",
				Variants:    variants,
				OriginKind:  ir.KindOpenAPI,
			}
			s.union = name
			op.Output = &ir.TypeRef{Named: name}
			op.OutputRequired = true
		}
		g.registry.Set(op.SchemaID, &streamDispatcher{s: s, emptyNilSlices: g.emptyNilSlices})
	}
	return nil
}

func exportedName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// streamDispatcher serves a streamOp as a GraphQL subscription.
type streamDispatcher struct {
	s              *streamOp
	emptyNilSlices bool
}

func (d *streamDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	inPtr := reflect.New(d.s.inputType)
	if err := bindInput(inPtr.Elem(), d.s.irOp, args, ""); err != nil {
		return nil, fmt.Errorf("gat: bind input for %s: %w", d.s.irOp.Name, err)
	}

	// graphql-go only streams a bidirectional chan interface{}.
	out := make(chan interface{})
	startedCh := make(chan struct{})
	doneCh := make(chan error, 1)
	emit := func(v any) error {
		ev, err := d.normalize(v)
		if err != nil {
			return err
		}
		select {
		case out <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() {
		defer close(out)
		doneCh <- d.s.run(ctx, inPtr.Interface(), func() { close(startedCh) }, emit)
	}()

	// The resolver must hand back the channel, but a rejection has to
	// be an error frame, not an empty stream: wait for the run to get
	// past its setup.
	select {
	case <-startedCh:
		return out, nil
	case err := <-doneCh:
		if err == nil {
			return out, nil // ended before starting; out is closed
		}
		// graphql-go's subscribe path formats a plain resolver error
		// without its Extensions (the query path keeps them), so hand
		// it an already-formatted error carrying status/code.
		ext := withStatusExtensions(err).(interface{ Extensions() map[string]any })
		return nil, gqlerrors.FormattedError{Message: err.Error(), Extensions: ext.Extensions()}
	}
}

// normalize turns one event into the generic JSON shape the runtime
// renders. A union event carries __typename so the union's
// ResolveType picks the variant by declared type, not by guessing
// from fields.
func (d *streamDispatcher) normalize(v any) (any, error) {
	var typename string
	if d.s.union != "" {
		e, err := d.s.eventFor(v)
		if err != nil {
			return nil, err
		}
		typename = e.schema
	}
	if d.emptyNilSlices {
		coerceNilSlices(reflect.ValueOf(v))
	}
	n, err := normalizeJSON(v)
	if err != nil {
		return nil, err
	}
	if typename != "" {
		if m, ok := n.(map[string]any); ok {
			m["__typename"] = typename
		}
	}
	return n, nil
}

// connectStream builds the gRPC server-stream handler for a streamOp.
// The run is synchronous here, so the middleware chain wraps the whole
// stream, as it does over REST.
func connectStream(s *streamOp, md protoreflect.MethodDescriptor) func(context.Context, *connect.Request[dynamicpb.Message], *connect.ServerStream[dynamicpb.Message]) error {
	bind := newConnectBinder(s.inputType, s.irOp, md)
	out := md.Output()
	return func(ctx context.Context, req *connect.Request[dynamicpb.Message], stream *connect.ServerStream[dynamicpb.Message]) error {
		in, err := bind(req.Msg)
		if err != nil {
			return err
		}
		ctx = withInboundRequest(ctx, connectInboundRequest(req))
		started := false
		emit := func(v any) error {
			body := v
			if s.union != "" {
				e, err := s.eventFor(v)
				if err != nil {
					return err
				}
				// The union renders as a oneof message; select the
				// member for this event's type.
				body = map[string]any{ir.ProtoUnionFieldName(e.schema): v}
			}
			msg := dynamicpb.NewMessage(out)
			if err := jsonToDynamic(body, msg); err != nil {
				return err
			}
			return stream.Send(msg)
		}
		err = s.run(ctx, in, func() { started = true }, emit)
		if err == nil || (started && errors.Is(err, context.Canceled)) {
			return nil
		}
		return handlerErrorToConnect(err)
	}
}
