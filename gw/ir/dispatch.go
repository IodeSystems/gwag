package ir

import (
	"context"
	"sync"
)

// SchemaID is the registry key for one IR Operation. The runtime
// renderer resolves an Operation to a Dispatcher by looking up its
// SchemaID in a DispatchRegistry; ingesters / registrars stamp
// SchemaIDs on Operations and register Dispatchers under the same
// id.
//
// Format: "<namespace>/<version>/<op>" where <op> is the flat
// operation name (path-joined by `_` for ops nested under Groups).
// The format is opaque to the registry — the convention exists so
// renderers can reconstruct the id from a Service tree without
// needing the original Operation pointer.
//
// Stability: stable
type SchemaID string

// Dispatcher executes one operation. Args are the IR-canonical
// argument map that any ingress (today only the GraphQL resolver)
// produces from the wire format. Result is the canonical response
// shape: map[string]any for objects, []any for lists, primitives
// otherwise. Errors propagate to the caller — middleware decides
// classification (resource-exhausted vs internal vs upstream).
//
// Stability: stable
type Dispatcher interface {
	Dispatch(ctx context.Context, args map[string]any) (any, error)
}

// AppendDispatcher is the optional capability interface for
// dispatchers that can emit their result as JSON bytes directly,
// bypassing the canonical-result allocation + the graphql-go
// leaf-emitter machinery. The renderer prefers DispatchAppend over
// Dispatch when a registered dispatcher implements both; falls back
// to Dispatch + graphql-go's serialization for plain Dispatchers.
//
// Contract:
//   - The implementation MUST append a complete, well-formed JSON
//     value matching the field's declared return type to dst and
//     return the extended slice. dst may grow; always use the
//     returned slice.
//   - For composite return types (Object, List), the emitted bytes
//     MUST cover the entire selection — the renderer does not
//     recurse into per-field resolvers when DispatchAppend handles
//     the field. Selection-aware emission is the implementer's job;
//     read the selection AST from rp.Info.FieldASTs (via the
//     graphql_dispatcher.go context plumbing in gateway code).
//   - Errors trigger the standard null-bubble-up: the renderer rolls
//     the field bytes back to entry length and records the error in
//     the response envelope.
//
// Middleware that wraps an AppendDispatcher and wants the append
// fast path to flow through MUST also implement AppendDispatcher
// (delegating DispatchAppend to the inner). Middleware that doesn't
// pass it through downgrades the chain to the Dispatch path with no
// correctness loss — just no perf win.
//
// Stability: stable
type AppendDispatcher interface {
	Dispatcher
	DispatchAppend(ctx context.Context, args map[string]any, dst []byte) ([]byte, error)
}

// DispatcherFunc adapts a plain function to Dispatcher.
//
// Stability: stable
type DispatcherFunc func(ctx context.Context, args map[string]any) (any, error)

// Dispatch implements Dispatcher by calling f directly.
//
// Stability: stable
func (f DispatcherFunc) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	return f(ctx, args)
}

// DispatcherMiddleware wraps a Dispatcher in another Dispatcher.
// Used for cross-cutting concerns (backpressure, metrics, auth)
// that today live inline in each format's resolver closure.
//
// Stability: stable
type DispatcherMiddleware func(next Dispatcher) Dispatcher

// DispatchRegistry maps SchemaID → Dispatcher. The runtime renderer
// builds resolvers that look up Dispatchers via SchemaID at call
// time, so the schema graph can be rebuilt independently of pool /
// source lifecycle. Lookups are read-mostly and concurrent.
//
// Stability: stable
type DispatchRegistry struct {
	mu          sync.RWMutex
	dispatchers map[SchemaID]Dispatcher
}

// NewDispatchRegistry returns an empty registry.
//
// Stability: stable
func NewDispatchRegistry() *DispatchRegistry {
	return &DispatchRegistry{dispatchers: map[SchemaID]Dispatcher{}}
}

// Set installs `d` under `id`. Replaces any existing entry.
//
// Stability: stable
func (r *DispatchRegistry) Set(id SchemaID, d Dispatcher) {
	r.mu.Lock()
	r.dispatchers[id] = d
	r.mu.Unlock()
}

// Get returns the Dispatcher registered under `id`, or nil if
// nothing is registered. Resolvers that call Get at dispatch time
// (rather than build time) tolerate pool / source churn between
// schema rebuilds.
//
// Stability: stable
func (r *DispatchRegistry) Get(id SchemaID) Dispatcher {
	r.mu.RLock()
	d := r.dispatchers[id]
	r.mu.RUnlock()
	return d
}

// Delete removes the dispatcher registered under `id`. No-op when
// nothing is registered.
//
// Stability: stable
func (r *DispatchRegistry) Delete(id SchemaID) {
	r.mu.Lock()
	delete(r.dispatchers, id)
	r.mu.Unlock()
}

// Len returns the number of registered dispatchers. Cheap; useful
// for tests and admin views.
//
// Stability: stable
func (r *DispatchRegistry) Len() int {
	r.mu.RLock()
	n := len(r.dispatchers)
	r.mu.RUnlock()
	return n
}

// MakeSchemaID returns the canonical SchemaID for an Operation
// living at `flatName` under (namespace, version). Use this anywhere
// a SchemaID needs to be reconstructed from a Service tree without
// the original Operation pointer (e.g. registrars wiring Dispatchers
// keyed by flat name).
//
// Stability: stable
func MakeSchemaID(namespace, version, flatName string) SchemaID {
	return SchemaID(namespace + "/" + version + "/" + flatName)
}

// Parts splits a SchemaID into its three components: namespace,
// version, and flat operation name. Empty strings come back when the
// id has fewer than two "/" separators; the registry treats the id as
// opaque, but tracing / logging sites benefit from the split.
//
// Stability: stable
func (id SchemaID) Parts() (namespace, version, op string) {
	s := string(id)
	first := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			first = i
			break
		}
	}
	if first < 0 {
		return "", "", s
	}
	rest := s[first+1:]
	second := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			second = i
			break
		}
	}
	if second < 0 {
		return s[:first], rest, ""
	}
	return s[:first], rest[:second], rest[second+1:]
}

// PopulateSchemaIDs walks every Operation and every OperationGroup
// reachable from `svc` (top-level + every Group descendant) and
// stamps SchemaID using MakeSchemaID. Operations use their flat
// path-joined name; Groups use `_group_<path>` so the IDs don't
// collide with leaf ops. Idempotent — call after Service.Namespace /
// Service.Version are set.
//
// Stability: stable
func PopulateSchemaIDs(svc *Service) {
	for _, op := range svc.Operations {
		op.SchemaID = MakeSchemaID(svc.Namespace, svc.Version, op.Name)
	}
	for _, g := range svc.Groups {
		populateGroupSchemaIDs(svc, g, "")
	}
}

func populateGroupSchemaIDs(svc *Service, g *OperationGroup, prefix string) {
	pre := prefix + g.Name + "_"
	g.SchemaID = MakeSchemaID(svc.Namespace, svc.Version, "_group_"+pre[:len(pre)-1])
	for _, op := range g.Operations {
		op.SchemaID = MakeSchemaID(svc.Namespace, svc.Version, pre+op.Name)
	}
	for _, sub := range g.Groups {
		populateGroupSchemaIDs(svc, sub, pre)
	}
}
