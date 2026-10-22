package gat

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

// RegisterSSE is the paired drop-in for sse.Register, as Register is
// for huma.Register. It registers the operation with huma exactly as
// sse.Register would (REST clients get Server-Sent Events, and the
// OpenAPI document describes the text/event-stream response), and
// captures the handler so gat serves the same stream as:
//
//   - a GraphQL subscription, Subscription.{Namespace}_{OperationID},
//     over graphql-transport-ws on {prefix}/graphql;
//   - a server-streaming gRPC method on RegisterGRPC.
//
// One event type in eventTypeMap: the field and the stream message
// are that type. Several: a union {OperationID}Event of the types —
// a GraphQL union (select with `... on T`) and a proto message with
// one oneof. Each sent value resolves to the variant of its Go type,
// which must be one of the declared types.
//
// On GraphQL and gRPC only Message.Data is delivered; ID and Retry
// are SSE wire details, and a comment-only message (a heartbeat) is
// skipped. The huma middleware chain (API stack, then
// op.Middlewares) wraps the handler on every surface. f's ctx is
// cancelled when the client completes or disconnects, and send then
// returns an error, as it does when an SSE client goes away.
//
// I takes query: and header: parameters; path: parameters work over
// REST but GraphQL and gRPC bind them as plain arguments. To reject a
// request on its input, implement huma.Resolver on I: as over REST,
// the input checks run after the middlewares and before f, so a
// rejection fails the request with its status instead of opening the
// stream.
//
// Must be called before RegisterHuma.
//
// Stability: experimental
func RegisterSSE[I any](api huma.API, g *Gateway, op huma.Operation, eventTypeMap map[string]any, f func(ctx context.Context, input *I, send sse.Sender)) {
	sse.Register(api, op, eventTypeMap, f)
	if g.built {
		return
	}
	if len(eventTypeMap) == 0 {
		panic(fmt.Sprintf("gat: RegisterSSE %s: eventTypeMap is empty", op.OperationID))
	}
	registry := api.OpenAPI().Components.Schemas
	events := make([]streamEvent, 0, len(eventTypeMap))
	for name, v := range eventTypeMap {
		t := derefType(reflect.TypeOf(v))
		// sse.Register already registered t; this returns its $ref.
		ref := registry.Schema(t, true, name).Ref
		events = append(events, streamEvent{
			name:   name,
			goType: t,
			schema: strings.TrimPrefix(ref, "#/components/schemas/"),
		})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].name < events[j].name })

	chain := opChain(api, op)
	opRef := op
	inputType := reflect.TypeOf((*I)(nil)).Elem()
	check := newInputCheck(api, op, inputType)
	run := func(ctx context.Context, in any, args map[string]any, started func(), emit func(any) error) error {
		typed, ok := in.(*I)
		if !ok {
			return fmt.Errorf("gat: input type mismatch for %s: got %T", op.OperationID, in)
		}
		return runChain(ctx, chain, &opRef, func(ctx context.Context) error {
			if err := check.apply(ctx, &opRef, typed, args); err != nil {
				return err
			}
			started()
			f(ctx, typed, func(m sse.Message) error {
				if m.Data == nil {
					return nil // comment / heartbeat: an SSE wire detail
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return emit(m.Data)
			})
			return nil
		})
	}
	g.subs = append(g.subs, &streamOp{
		op:        op,
		inputType: inputType,
		events:    events,
		run:       run,
		check:     check,
	})
}
