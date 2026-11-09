package gat

import (
	"encoding/json"
	"net/http"

	"github.com/IodeSystems/graphql-go/v2"
	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// schemaGraphQLHandler serves the gateway's GraphQL surface as SDL
// (default) or introspection JSON (`?format=json`). Codegen tools can
// point straight at this URL.
func schemaGraphQLHandler(g *Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST → delegate to the GraphQL execution handler. graphql-codegen
		// and similar tools introspect via a POST query; pointing them at
		// this documented schema URL then "just works" instead of 405-ing.
		if r.Method == http.MethodPost {
			g.Handler().ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap := g.snap.Load()
		if snap == nil {
			http.Error(w, "schema not assembled", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Query().Get("format") {
		case "json":
			result := graphql.Do(graphql.Params{
				Schema:        *snap.schema,
				RequestString: ir.IntrospectionQuery,
			})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = ir.WriteJSON(w, result)
		default:
			w.Header().Set("Content-Type", "application/graphql; charset=utf-8")
			_, _ = w.Write([]byte(ir.PrintSchemaSDL(snap.schema, snap.annotations)))
		}
	})
}

// schemaProtoHandler renders gat's IR services as a FileDescriptorSet
// (default, `application/protobuf`) so proto-codegen tools can consume
// the same surface as GraphQL. `?service=` / `?version=` select
// services (serviceFilter); a deprecated version's file, services and
// methods carry the deprecated option.
func schemaProtoHandler(g *Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		filter, err := parseServiceFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		all := g.Services()
		services := filter.apply(all)
		if filter.set() && len(services) == 0 {
			http.Error(w, "no service matches", http.StatusNotFound)
			return
		}
		fds, err := renderProto(services, deprecations(all))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(fds)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		w.Header().Set("Content-Disposition", `attachment; filename="services.fds"`)
		_, _ = w.Write(out)
	})
}

// schemaOpenAPIHandler re-emits the OpenAPI projection of every IR
// service. When the source was OpenAPI (e.g. huma's spec) the round
// trip is near-identity; when the source was proto/GraphQL the
// projection produces a synthesized spec. `?openapi=3.0|3.1` writes
// every spec in that version, converting where needed; the default is
// OpenAPIVersion, else each spec as declared.
//
// `?service=` / `?version=` select services (serviceFilter). One
// selected service is emitted as its spec; several as a map keyed by
// namespace, or, when a namespace has several versions selected, by
// namespace then version (gwag's shape). A deprecated version's
// operations carry `deprecated: true`.
func schemaOpenAPIHandler(g *Gateway) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		opts, err := ir.ParseOpenAPIVersion(r.URL.Query().Get("openapi"), g.openAPIVersion)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		filter, err := parseServiceFilter(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		all := g.Services()
		services := filter.apply(all)
		if len(services) == 0 {
			if filter.set() && len(all) > 0 {
				http.Error(w, "no service matches", http.StatusNotFound)
				return
			}
			http.Error(w, "no services", http.StatusServiceUnavailable)
			return
		}
		deps := deprecations(all)
		render := func(svc *ir.Service) (any, error) {
			doc, err := ir.RenderOpenAPIWith(svc, opts)
			if err != nil || deps[svc] == "" {
				return doc, err
			}
			return deprecateOpenAPI(doc)
		}
		// Single-service case: emit the one spec directly.
		if len(services) == 1 {
			doc, err := render(services[0])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		perNS := map[string]int{}
		versioned := false
		for _, svc := range services {
			perNS[svc.Namespace]++
			versioned = versioned || perNS[svc.Namespace] > 1
		}
		// Multi-service: emit a map keyed by namespace (and version).
		bundle := map[string]any{}
		for _, svc := range services {
			doc, err := render(svc)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !versioned {
				bundle[svc.Namespace] = doc
				continue
			}
			byVer, _ := bundle[svc.Namespace].(map[string]any)
			if byVer == nil {
				byVer = map[string]any{}
				bundle[svc.Namespace] = byVer
			}
			byVer[svc.Version] = doc
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(bundle)
	})
}
