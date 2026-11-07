package gateway

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// wrapCanonicalDispatcherWithChain returns an ir.Dispatcher that
// runs `chain` over a synthesised dynamicpb input message before
// handing canonical args off to `inner`. Boundary conversion lets
// the proto-shape Middleware chain (InjectType / InjectPath / user
// runtime hooks) fire for openapi- and graphql-ingested ops too,
// closing the cross-format runtime gap noted in plan §1 followups.
//
// Limit: argsToMessage / messageToMap key by lowerCamel(fd.Name()).
// IR Arg.Name flows verbatim into the synth field name, so an arg
// named "user_id" becomes proto field "user_id" with json key
// "userId" — the canonical args map (graphql resolver shape) holds
// "user_id" instead and the boundary roundtrip drops it. Accepted
// limitation: the typical lowerCamel/camelCase conventions used by
// graphql + most openapi specs come through cleanly.
//
// If `inner` returns an error after the chain has already mutated
// the request fields, the chain's mutations are observable via the
// closure's modifiedArgs but go nowhere — by that point inner has
// already declined the request. That matches proto-side semantics.
func wrapCanonicalDispatcherWithChain(inner ir.Dispatcher, chain Middleware, inputDesc protoreflect.MessageDescriptor, namespace, version, opName string) ir.Dispatcher {
	if inputDesc == nil {
		return inner
	}
	d := &canonicalChainedDispatcher{
		inner:     inner,
		chain:     chain,
		inputDesc: inputDesc,
		namespace: namespace,
		version:   version,
		op:        opName,
	}
	// Only claim the append capability when inner has it: the renderer
	// treats an AppendDispatcher that cannot append as a hard error.
	if ad, ok := inner.(ir.AppendDispatcher); ok {
		return &canonicalChainedAppendDispatcher{canonicalChainedDispatcher: d, innerAppend: ad}
	}
	return d
}

type canonicalChainedDispatcher struct {
	inner     ir.Dispatcher
	chain     Middleware
	inputDesc protoreflect.MessageDescriptor
	namespace string
	version   string
	op        string
}

func (d *canonicalChainedDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	var result any
	err := d.run(ctx, args, func(ctx context.Context, a map[string]any) (err error) {
		result, err = d.inner.Dispatch(ctx, a)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// canonicalChainedAppendDispatcher keeps the byte-splice fast path
// alive through the runtime chain: the chain still sees and may mutate
// the proto-shape request, then the post-chain args go to
// innerAppend.DispatchAppend.
type canonicalChainedAppendDispatcher struct {
	*canonicalChainedDispatcher
	innerAppend ir.AppendDispatcher
}

func (d *canonicalChainedAppendDispatcher) DispatchAppend(ctx context.Context, args map[string]any, dst []byte) ([]byte, error) {
	out := dst
	err := d.run(ctx, args, func(ctx context.Context, a map[string]any) (err error) {
		out, err = d.innerAppend.DispatchAppend(ctx, a, dst)
		return err
	})
	if err != nil {
		return dst, err
	}
	return out, nil
}

// run executes the runtime chain over args and invokes call with the
// post-chain canonical args as the terminal step.
func (d *canonicalChainedDispatcher) run(ctx context.Context, args map[string]any, call func(context.Context, map[string]any) error) error {
	ctx = withDispatchOpInfo(ctx, d.namespace, d.version, d.op)
	req := acquireDynamicMessage(d.inputDesc)
	defer releaseDynamicMessage(d.inputDesc, req)
	if err := argsToMessage(args, req); err != nil {
		return fmt.Errorf("runtime chain: encode args: %w", err)
	}

	// The terminal handler runs last: by then the chain has applied
	// its mutations to req. call's result is captured by the closure
	// since the chain is proto-typed.
	var resultErr error
	terminal := Handler(func(ctx context.Context, postChain protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
		dyn, ok := postChain.(*dynamicpb.Message)
		if !ok {
			resultErr = call(ctx, args)
			return postChain, resultErr
		}
		modified := messageToMap(dyn)
		// Preserve args that didn't survive lowerCamel roundtripping —
		// pass them through so dispatch logic that reads a snake_case
		// path/query param still finds it.
		for k, v := range args {
			if _, ok := modified[k]; ok {
				continue
			}
			modified[k] = v
		}
		// Mark the args as authoritative so a graphql-ingest
		// dispatcher binds the top-level arguments from `modified`
		// rather than forwarding the caller's argument ASTs (which
		// carry the pre-mutation literals). The forward info stays:
		// both OpenAPI and GraphQL dispatchers need the caller's
		// selection to shape the response.
		resultErr = call(withChainArgs(ctx), modified)
		return postChain, resultErr
	})

	chained := d.chain(terminal)
	if _, err := chained(ctx, req); err != nil {
		return err
	}
	return resultErr
}

var _ ir.Dispatcher = (*canonicalChainedDispatcher)(nil)
var _ ir.AppendDispatcher = (*canonicalChainedAppendDispatcher)(nil)
var _ ir.Dispatcher = (*canonicalChainedDispatcher)(nil)

// hasRuntimeMiddleware reports whether any registered Transform
// carries a non-nil Runtime middleware. Used at register time to
// skip the boundary-conversion wrap when the chain would be a
// no-op. Caller holds g.mu.
func (g *Gateway) hasRuntimeMiddleware() bool {
	for _, t := range g.transforms {
		if t.Runtime != nil {
			return true
		}
	}
	return false
}

// buildOpInputDescriptorsLocked renders `svcs` to a FileDescriptorSet
// and returns one `protoreflect.MessageDescriptor` per Operation,
// keyed by its SchemaID. The rendered methods are named after the
// flat operation name (FlatOperations / PopulateSchemaIDs share that
// space), so the keys line up with `op.SchemaID` regardless of
// nesting.
//
// Returns nil on synthesis errors (file collision, invalid
// descriptor); callers fall back to the un-wrapped dispatcher.
// Caller holds g.mu.
func (g *Gateway) buildOpInputDescriptorsLocked(svcs []*ir.Service) map[ir.SchemaID]protoreflect.MessageDescriptor {
	out := map[ir.SchemaID]protoreflect.MessageDescriptor{}
	for _, svc := range svcs {
		fds, err := ir.RenderProtoFiles([]*ir.Service{svc})
		if err != nil || fds == nil || len(fds.File) == 0 {
			continue
		}
		files, err := protodesc.NewFiles(fds)
		if err != nil {
			continue
		}
		methodByName := map[string]protoreflect.MethodDescriptor{}
		for _, fp := range fds.File {
			fd, err := files.FindFileByPath(fp.GetName())
			if err != nil {
				continue
			}
			ss := fd.Services()
			for i := 0; i < ss.Len(); i++ {
				ms := ss.Get(i).Methods()
				for j := 0; j < ms.Len(); j++ {
					md := ms.Get(j)
					methodByName[string(md.Name())] = md
				}
			}
		}
		for _, op := range svc.FlatOperations() {
			md, ok := methodByName[op.Name]
			if !ok {
				continue
			}
			out[op.SchemaID] = md.Input()
		}
	}
	return out
}
