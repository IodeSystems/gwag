package gat

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
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

// Subscribe registers a GraphQL subscription field backed by source.
// I is the argument struct (huma `query:"…"` and `header:"…"` tags;
// path parameters do not exist here), O is one event. The field
// renders as Subscription.{Namespace}_{OperationID}, served over
// graphql-transport-ws on {prefix}/graphql, and as a server-streaming
// method on RegisterGRPC.
//
// source is called once per subscription, after the huma middleware
// chain (API stack, then op.Middlewares) has passed; ctx carries what
// the middlewares attached and is cancelled when the client completes
// or disconnects. source owns the returned channel: it must close it
// when the stream ends, and must stop sending once ctx is done.
//
// Only OperationID, Summary, Description, Tags, Deprecated, and
// Middlewares are read from op. The subscription is not mounted on
// the adopter's REST router.
//
// Must be called before RegisterHuma.
//
// Deprecated: use RegisterSSE. It defines the stream in huma
// (sse.Register), so the same handler also serves REST clients as
// Server-Sent Events, and it supports several event types.
//
// Stability: experimental
func Subscribe[I, O any](api huma.API, g *Gateway, op huma.Operation, source func(context.Context, *I) (<-chan *O, error)) {
	if g.built {
		return
	}
	if op.OperationID == "" {
		panic("gat: Subscribe requires an OperationID")
	}
	chain := opChain(api, op)

	op.Method = http.MethodGet
	op.Path = subscriptionPathPrefix + op.OperationID
	op.Middlewares = nil
	huma.Register(g.subscriptionAPI(api), op, func(context.Context, *I) (*subscriptionOutput[O], error) {
		return nil, huma.Error501NotImplemented("subscription")
	})

	opRef := op
	run := func(ctx context.Context, in any, started func(), emit func(any) error) error {
		typed, ok := in.(*I)
		if !ok {
			return fmt.Errorf("gat: input type mismatch for %s: got %T", op.OperationID, in)
		}
		return runChain(ctx, chain, &opRef, func(ctx context.Context) error {
			src, err := source(ctx, typed)
			if err != nil {
				return err
			}
			started()
			for {
				select {
				case <-ctx.Done():
					return nil
				case ev, ok := <-src:
					if !ok {
						return nil
					}
					if err := emit(ev); err != nil {
						return nil // consumer gone
					}
				}
			}
		})
	}
	g.subs = append(g.subs, &streamOp{
		op:        op,
		inputType: reflect.TypeOf((*I)(nil)).Elem(),
		run:       run,
	})
}

// opChain is the middleware chain huma.Register applies to op on api:
// API stack first, then the op's own. Captured when the op is
// registered, as huma does.
func opChain(api huma.API, op huma.Operation) huma.Middlewares {
	var chain huma.Middlewares
	chain = append(chain, api.Middlewares()...)
	chain = append(chain, op.Middlewares...)
	return chain
}

// runChain runs call behind chain (directly when chain is empty).
func runChain(ctx context.Context, chain huma.Middlewares, op *huma.Operation, call func(context.Context) error) error {
	if len(chain) == 0 {
		return call(ctx)
	}
	_, err := runHumaMiddlewares(ctx, chain, op, func(ctx context.Context) (any, error) {
		return nil, call(ctx)
	})
	return err
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
// Subscribe ops' paths added. A shallow copy: the adopter's served
// document is not modified.
func (g *Gateway) mergedOpenAPI(api huma.API) *huma.OpenAPI {
	doc := api.OpenAPI()
	if g.subAPI == nil {
		return doc
	}
	merged := *doc
	merged.Paths = make(map[string]*huma.PathItem, len(doc.Paths)+len(g.subAPI.OpenAPI().Paths))
	for p, item := range doc.Paths {
		merged.Paths[p] = item
	}
	for p, item := range g.subAPI.OpenAPI().Paths {
		merged.Paths[p] = item
	}
	return &merged
}
