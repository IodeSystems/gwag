package gat

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/IodeSystems/graphql-go/v2/gqlerrors"
	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// subscriptionPathPrefix is where Subscribe places its synthetic
// operations in the side API's OpenAPI document. They never reach the
// adopter's router or REST spec; the path only has to be unique.
const subscriptionPathPrefix = "/_gat/subscriptions/"

// subscriptionOutput wraps an event type in huma's output convention
// (the JSON body lives in Body) so huma derives O's schema the same way
// it does for a Register output.
type subscriptionOutput[O any] struct {
	Body O
}

// capturedSub is one Subscribe entry: the huma op (with the synthetic
// method/path), input reflection, and a type-erased source that
// returns events as chan any. irOp is set by ingestHuma.
type capturedSub struct {
	op        huma.Operation
	inputType reflect.Type
	open      func(ctx context.Context, in any) (<-chan any, error)
	irOp      *ir.Operation
}

// Subscribe registers a GraphQL subscription field backed by source.
// It is the streaming counterpart of Register: I is the argument
// struct (huma `query:"…"` and `header:"…"` tags; path parameters do
// not exist here), O is one event. The field renders as
// Subscription.{Namespace}_{OperationID}, served over
// graphql-transport-ws on {prefix}/graphql.
//
// source is called once per subscription, after the huma middleware
// chain (API stack, then op.Middlewares) has passed; ctx carries what
// the middlewares attached and is cancelled when the client completes
// or disconnects. source owns the returned channel: it must close it
// when the stream ends, and must stop sending once ctx is done. Replay
// (events after a cursor) is the source's job — read history, then
// switch to live, inside the one channel.
//
// Only OperationID, Summary, Description, Tags, Deprecated, and
// Middlewares are read from op. The subscription is not mounted on
// the adopter's REST router.
//
// Must be called before RegisterHuma.
//
// Stability: experimental
func Subscribe[I, O any](api huma.API, g *Gateway, op huma.Operation, source func(context.Context, *I) (<-chan *O, error)) {
	if g.built {
		return
	}
	if op.OperationID == "" {
		panic("gat: Subscribe requires an OperationID")
	}
	var chain huma.Middlewares
	chain = append(chain, api.Middlewares()...)
	chain = append(chain, op.Middlewares...)

	op.Method = http.MethodGet
	op.Path = subscriptionPathPrefix + op.OperationID
	op.Middlewares = nil
	huma.Register(g.subscriptionAPI(api), op, func(context.Context, *I) (*subscriptionOutput[O], error) {
		return nil, huma.Error501NotImplemented("subscription")
	})

	opRef := op
	open := func(ctx context.Context, in any) (<-chan any, error) {
		typed, ok := in.(*I)
		if !ok {
			return nil, fmt.Errorf("gat: input type mismatch for %s: got %T", op.OperationID, in)
		}
		call := func(ctx context.Context) (any, error) {
			src, err := source(ctx, typed)
			if err != nil {
				return nil, err
			}
			return src, nil
		}
		var res any
		var err error
		if len(chain) == 0 {
			res, err = call(ctx)
		} else {
			res, err = runHumaMiddlewares(ctx, chain, &opRef, call)
		}
		if err != nil {
			return nil, err
		}
		return pumpEvents(ctx, res.(<-chan *O)), nil
	}
	g.subs = append(g.subs, &capturedSub{
		op:        op,
		inputType: reflect.TypeOf((*I)(nil)).Elem(),
		open:      open,
	})
}

// pumpEvents re-types a source channel as chan any for the GraphQL
// runtime. Ends when the source closes or ctx is done.
func pumpEvents[O any](ctx context.Context, src <-chan *O) <-chan any {
	out := make(chan any)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// subscriptionAPI returns the side huma API that Subscribe registers
// its synthetic operations on. It shares the adopter API's schema
// registry, so event types reuse (and are named like) the adopter's
// own types, but has no routes and no OpenAPI endpoint.
func (g *Gateway) subscriptionAPI(api huma.API) huma.API {
	if g.subAPI != nil {
		return g.subAPI
	}
	cfg := huma.DefaultConfig("gat-subscriptions", "v1")
	cfg.OpenAPIPath, cfg.DocsPath, cfg.SchemasPath = "", "", ""
	// No create hooks: the default schema-link hook would add a
	// `$schema` property to event types in the shared registry.
	cfg.CreateHooks = nil
	side := huma.NewAPI(cfg, noRoutesAdapter{})
	// Share the registry after construction. Passing it through cfg
	// would make NewAPI overwrite the adopter registry's config.
	side.OpenAPI().Components = api.OpenAPI().Components
	g.subAPI = side
	return side
}

// noRoutesAdapter discards route registrations: subscription ops exist
// only for their OpenAPI description.
type noRoutesAdapter struct{}

func (noRoutesAdapter) Handle(*huma.Operation, func(huma.Context)) {}
func (noRoutesAdapter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.NotFound(w, nil)
}

// mergedOpenAPI returns the adopter's OpenAPI document with the
// subscription ops' paths added. A shallow copy: the adopter's served
// document is not modified.
func (g *Gateway) mergedOpenAPI(api huma.API) *huma.OpenAPI {
	doc := api.OpenAPI()
	if g.subAPI == nil {
		return doc
	}
	merged := *doc
	merged.Paths = make(map[string]*huma.PathItem, len(doc.Paths)+len(g.subs))
	for p, item := range doc.Paths {
		merged.Paths[p] = item
	}
	for p, item := range g.subAPI.OpenAPI().Paths {
		merged.Paths[p] = item
	}
	return &merged
}

// subDispatcher opens one stream per GraphQL subscription: binds the
// field args into *I, runs the source, and normalizes each event to
// the generic JSON shape the runtime renders.
type subDispatcher struct {
	sub            *capturedSub
	op             *ir.Operation
	emptyNilSlices bool
}

func (d *subDispatcher) Dispatch(ctx context.Context, args map[string]any) (any, error) {
	inPtr := reflect.New(d.sub.inputType)
	if err := bindInput(inPtr.Elem(), d.op, args, ""); err != nil {
		return nil, fmt.Errorf("gat: bind input for %s: %w", d.op.Name, err)
	}
	events, err := d.sub.open(ctx, inPtr.Interface())
	if err != nil {
		// graphql-go's subscribe path formats a plain resolver error
		// without its Extensions (the query path keeps them), so hand
		// it an already-formatted error carrying status/code.
		ext := withStatusExtensions(err).(interface{ Extensions() map[string]any })
		return nil, gqlerrors.FormattedError{Message: err.Error(), Extensions: ext.Extensions()}
	}
	// graphql-go only streams a bidirectional chan interface{}.
	out := make(chan interface{})
	go func() {
		defer close(out)
		for ev := range events {
			if d.emptyNilSlices {
				coerceNilSlices(reflect.ValueOf(ev))
			}
			v, err := normalizeJSON(ev)
			if err != nil {
				continue
			}
			select {
			case out <- v:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// bindSubscriptions marks the ingested synthetic ops as subscriptions
// and wires their dispatchers. Called from ingestHuma.
func (g *Gateway) bindSubscriptions(svc *ir.Service) error {
	if len(g.subs) == 0 {
		return nil
	}
	byID := make(map[string]*capturedSub, len(g.subs))
	for _, s := range g.subs {
		byID[s.op.OperationID] = s
	}
	for _, op := range svc.FlatOperations() {
		s, ok := byID[op.Name]
		if !ok || !strings.HasPrefix(op.HTTPPath, subscriptionPathPrefix) {
			continue
		}
		op.Kind = ir.OpSubscription
		s.irOp = op
		g.registry.Set(op.SchemaID, &subDispatcher{sub: s, op: op, emptyNilSlices: g.emptyNilSlices})
	}
	for _, s := range g.subs {
		if s.irOp == nil {
			return fmt.Errorf("gat: subscription %s missing from ingested schema", s.op.OperationID)
		}
	}
	return nil
}
