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
type SchemaID string

// Dispatcher executes one operation. Args are the IR-canonical
// argument map that any ingress (today only the GraphQL resolver)
// produces from the wire format. Result is the canonical response
// shape: map[string]any for objects, []any for lists, primitives
// otherwise. Errors propagate to the caller — middleware decides
// classification (resource-exhausted vs internal vs upstream).
type Dispatcher interface {
	Dispatch(ctx context.Context, args map[string]any) (any, error)
}

// DispatcherFunc adapts a plain function to Dispatcher.
type DispatcherFunc func(ctx context.Context, args map[string]any) (any, error)

func (f DispatcherFunc) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	return f(ctx, args)
}

// DispatcherMiddleware wraps a Dispatcher in another Dispatcher.
// Used for cross-cutting concerns (backpressure, metrics, auth)
// that today live inline in each format's resolver closure.
type DispatcherMiddleware func(next Dispatcher) Dispatcher

// DispatchRegistry maps SchemaID → Dispatcher. The runtime renderer
// builds resolvers that look up Dispatchers via SchemaID at call
// time, so the schema graph can be rebuilt independently of pool /
// source lifecycle. Lookups are read-mostly and concurrent.
type DispatchRegistry struct {
	mu          sync.RWMutex
	dispatchers map[SchemaID]Dispatcher
}

// NewDispatchRegistry returns an empty registry.
func NewDispatchRegistry() *DispatchRegistry {
	return &DispatchRegistry{dispatchers: map[SchemaID]Dispatcher{}}
}

// Set installs `d` under `id`. Replaces any existing entry.
func (r *DispatchRegistry) Set(id SchemaID, d Dispatcher) {
	r.mu.Lock()
	r.dispatchers[id] = d
	r.mu.Unlock()
}

// Get returns the Dispatcher registered under `id`, or nil if
// nothing is registered. Resolvers that call Get at dispatch time
// (rather than build time) tolerate pool / source churn between
// schema rebuilds.
func (r *DispatchRegistry) Get(id SchemaID) Dispatcher {
	r.mu.RLock()
	d := r.dispatchers[id]
	r.mu.RUnlock()
	return d
}

// Delete removes the dispatcher registered under `id`. No-op when
// nothing is registered.
func (r *DispatchRegistry) Delete(id SchemaID) {
	r.mu.Lock()
	delete(r.dispatchers, id)
	r.mu.Unlock()
}

// Len returns the number of registered dispatchers. Cheap; useful
// for tests and admin views.
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
func MakeSchemaID(namespace, version, flatName string) SchemaID {
	return SchemaID(namespace + "/" + version + "/" + flatName)
}

// PopulateSchemaIDs walks every Operation reachable from `svc`
// (top-level + every Group descendant) and stamps SchemaID using
// MakeSchemaID with the flat path-joined name. Idempotent — call
// after Service.Namespace / Service.Version are set.
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
	for _, op := range g.Operations {
		op.SchemaID = MakeSchemaID(svc.Namespace, svc.Version, pre+op.Name)
	}
	for _, sub := range g.Groups {
		populateGroupSchemaIDs(svc, sub, pre)
	}
}
