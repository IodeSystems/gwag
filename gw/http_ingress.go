package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// ingressRoute is one resolved (METHOD, path) → dispatcher entry. The
// dispatcher pointer is captured at build time; the route table is
// rebuilt on every assembleLocked, so dispatcher churn doesn't strand
// stale entries.
//
// Today only the proto-style shape is populated — POST against the
// proto wire path /<pkg>.<Service>/<method> with a JSON body whose
// fields are the canonical args. OpenAPI's HTTPMethod / HTTPPath
// shape is the next commit.
type ingressRoute struct {
	method     string
	path       string // exact match for proto-style; "" when templated
	schemaID   ir.SchemaID
	dispatcher ir.Dispatcher
	shape      ingressShape
}

type ingressShape int

const (
	// ingressShapeProtoPost: POST /<pkg>.<Service>/<method> with a
	// JSON object whose top-level keys are the canonical args
	// (lowerCamel of proto fields). Result is wrapped as
	// {"result": <map>} so empty-object dispatches still produce
	// well-formed JSON.
	ingressShapeProtoPost ingressShape = iota
)

// ingressTable holds the route set assembled for the current schema.
// Lookups are O(1) by (method, path) for proto-style routes; templated
// routes (OpenAPI) will plug into a separate slice scanned on miss.
type ingressTable struct {
	exact map[string]*ingressRoute // key: METHOD + " " + path
}

// rebuildIngressLocked walks every proto pool's RPCs and emits one
// ingressShapeProtoPost route per unary method, pointing at the
// dispatcher already registered in g.dispatchers. Caller holds g.mu.
//
// Server-streaming methods are skipped — subscriptions live on a
// separate transport (graphql-ws today; HTTP/SSE planned). Internal
// namespaces (`_*` or AsInternal) are skipped just like the GraphQL
// surface skips them.
func (g *Gateway) rebuildIngressLocked() {
	t := &ingressTable{exact: map[string]*ingressRoute{}}
	for _, p := range g.pools {
		if g.isInternal(p.key.namespace) {
			continue
		}
		services := p.file.Services()
		for i := 0; i < services.Len(); i++ {
			sd := services.Get(i)
			methods := sd.Methods()
			for j := 0; j < methods.Len(); j++ {
				md := methods.Get(j)
				if md.IsStreamingClient() || md.IsStreamingServer() {
					continue
				}
				sid := ir.MakeSchemaID(p.key.namespace, p.key.version, string(md.Name()))
				d := g.dispatchers.Get(sid)
				if d == nil {
					continue
				}
				path := fmt.Sprintf("/%s/%s", sd.FullName(), md.Name())
				t.exact[http.MethodPost+" "+path] = &ingressRoute{
					method:     http.MethodPost,
					path:       path,
					schemaID:   sid,
					dispatcher: d,
					shape:      ingressShapeProtoPost,
				}
			}
		}
	}
	g.ingressRoutes.Store(t)
}

// IngressHandler returns an http.Handler that accepts inbound requests
// in the registered services' native shape — today, proto-style POSTs
// at /<pkg>.<Service>/<method> with a JSON body that mirrors the
// proto request message in canonical-args form. The handler dispatches
// through the same ir.Dispatcher chain the GraphQL resolver does, so
// runtime middleware (Hides, HideAndInject, user Pairs) and
// per-pool backpressure apply identically.
//
// Mount alongside Handler() (e.g. on /api/grpc/* or as the catch-all
// for the proto-style URL space). Unmatched paths return 404. The
// handler is safe to call before Handler() — it triggers schema
// assembly the same way.
func (g *Gateway) IngressHandler() http.Handler {
	g.mu.Lock()
	if g.schema.Load() == nil {
		if err := g.assembleLocked(); err != nil {
			g.mu.Unlock()
			return errorHandler(err)
		}
	}
	g.mu.Unlock()
	return http.HandlerFunc(g.serveIngress)
}

func (g *Gateway) serveIngress(w http.ResponseWriter, r *http.Request) {
	if g.draining.Load() {
		writeIngressError(w, http.StatusServiceUnavailable, "draining")
		return
	}
	t := g.ingressRoutes.Load()
	if t == nil {
		writeIngressError(w, http.StatusNotFound, "no routes")
		return
	}
	route, ok := t.exact[r.Method+" "+r.URL.Path]
	if !ok {
		writeIngressError(w, http.StatusNotFound, "no route for "+r.Method+" "+r.URL.Path)
		return
	}

	args, err := decodeIngressArgs(r, route.shape)
	if err != nil {
		writeIngressError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := withInjectCache(r.Context())
	ctx = WithHTTPRequest(ctx, r)

	out, err := route.dispatcher.Dispatch(ctx, args)
	if err != nil {
		writeIngressDispatchError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		// Body already partially written by Encode if it got past the
		// header; nothing useful to recover.
		return
	}
}

// decodeIngressArgs reads the request body for a proto-style POST and
// returns the canonical args map. Empty body is allowed (zero args).
// Bodies decoding to anything but a JSON object are rejected so the
// dispatcher contract stays unambiguous.
//
// Content-Type is permissive: any application/json* variant is fine,
// missing header is treated as JSON. The dispatcher's argsToMessage
// will surface field-level mismatches as INVALID_ARGUMENT rejections.
func decodeIngressArgs(r *http.Request, shape ingressShape) (map[string]any, error) {
	switch shape {
	case ingressShapeProtoPost:
		const limit = 10 << 20 // 10 MiB
		dec := json.NewDecoder(io.LimitReader(r.Body, limit+1))
		dec.UseNumber()
		var args map[string]any
		if err := dec.Decode(&args); err != nil {
			if err == io.EOF {
				return map[string]any{}, nil
			}
			return nil, fmt.Errorf("decode body: %w", err)
		}
		if args == nil {
			args = map[string]any{}
		}
		return args, nil
	}
	return nil, fmt.Errorf("unsupported ingress shape %d", shape)
}

// writeIngressError writes a JSON error envelope and the given status.
// Mirrors what the GraphQL handler returns on rejections so codegen
// clients can share their error-decode path.
func writeIngressError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// writeIngressDispatchError maps a Reject (or arbitrary error) onto an
// HTTP status. Uses the gateway's Code enum the same way the OpenAPI
// dispatcher's classification does — keeps ingress + egress error
// codes symmetric.
func writeIngressDispatchError(w http.ResponseWriter, err error) {
	var rej *rejection
	if errors.As(err, &rej) {
		writeIngressError(w, codeToHTTPStatus(rej.Code), rej.Msg)
		return
	}
	writeIngressError(w, http.StatusInternalServerError, err.Error())
}

// codeToHTTPStatus is the inverse of httpStatusToCode in openapi.go —
// classifies a gateway Code onto the most appropriate HTTP status for
// ingress error envelopes.
func codeToHTTPStatus(c Code) int {
	switch c {
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

