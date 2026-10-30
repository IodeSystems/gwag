package gateway

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// newGraphQLIngress builds the gat gateway that serves Handler. gat
// owns the request path (parsing, batches, trusted documents, plan,
// execute, graphql-transport-ws); gwag supplies the schema
// (assembleLocked → SetSchema), its plan cache and limits, and the
// hooks below for tracing, metrics, the request log, uploads, the
// WebSocket limits and drain, and GraphiQL. The same gat gateway
// serves the gRPC ingress (grpc_ingress.go).
func (g *Gateway) newGraphQLIngress() *gat.Gateway {
	cfg := g.cfg
	g.graphqlDone = g.graphqlRequestDone
	g.grpcDone = g.grpcUnaryDone
	gg, _ := gat.New()
	gg.UsePlanCache(g.planCache).
		MaxRequestBytes(cfg.maxRequestBytes).
		// Caps a batch's length; the per-document cap is the plan
		// cache's MaxOperationsRule.
		MaxOperations(cfg.maxOperations).
		AcceptStringVariables(true).
		// gwag has always answered a client's complete with one.
		EchoComplete(true).
		PreRequest(g.graphqlPreRequest).
		// The gRPC ingress (grpc_ingress.go) runs on gat too.
		PreCall(g.grpcPreCall).
		WSUpgrade(g.graphqlWSUpgrade).
		ParseContentType(contentTypeMultipartForm, cfg.uploadLimit, parseMultipartRequest).
		ParseContentType(contentTypeGraphQL, cfg.maxRequestBytes, parseGraphQLBody).
		ParseContentType(contentTypeFormURLEncoded, cfg.maxRequestBytes, parseFormRequest)
	if cfg.trusted != nil {
		gg.TrustedDocuments(cfg.trusted.docs, cfg.trusted.enforce)
	}
	if !cfg.disableGraphiQL {
		gg.GraphiQL(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if gh := g.graphiqlHandler.Load(); gh != nil {
				gh.ServeHTTP(w, r)
			}
		}))
	}
	return gg
}

// graphqlPreRequest wraps every GraphQL HTTP request: trace span,
// dispatch-time accumulator, inject cache and the inbound request on
// the context; request metrics and the request log when it is done.
func (g *Gateway) graphqlPreRequest(w http.ResponseWriter, r *http.Request) (context.Context, func(context.Context), bool) {
	ctx := g.tracer.extractHTTP(r.Context(), r.Header)
	// The span rides on ctx; graphqlRequestDone ends it.
	ctx, _ = g.tracer.startIngressSpan(ctx, "gateway.graphql",
		ingressAttr("graphql"),
		httpMethodAttr(r.Method),
		httpTargetAttr(r.URL.Path),
	)
	ctx = withTracer(ctx, g.tracer)
	ctx, accum := withDispatchAccumulator(ctx)
	ctx = withInjectCache(ctx)
	ctx = withHTTPRequest(ctx, r)
	accum.start = time.Now()
	return ctx, g.graphqlDone, true
}

// graphqlRequestDone records a finished GraphQL HTTP request. It reads
// what graphqlPreRequest put on ctx, so one func serves every request
// (g.graphqlDone) instead of a closure per request.
func (g *Gateway) graphqlRequestDone(ctx context.Context) {
	accum := dispatchAccumulatorFromContext(ctx)
	total := time.Since(accum.start)
	dispatchSum := time.Duration(accum.Sum.Load())
	g.cfg.metrics.RecordRequest("graphql", total, total-dispatchSum)
	g.logRequestLine("graphql", HTTPRequestFromContext(ctx).URL.Path, total, dispatchSum, int(accum.Count.Load()))
	oteltrace.SpanFromContext(ctx).End()
}

// graphqlWSUpgrade admits a graphql-transport-ws upgrade: the per-IP
// limit (WithWSLimit), refusal while draining, and registration so
// Drain can close the connection. WebSocket connections record no
// request metrics: request_*_seconds is per request, not per stream
// lifetime.
func (g *Gateway) graphqlWSUpgrade(w http.ResponseWriter, r *http.Request) (context.Context, func(context.Context), bool) {
	release, reason, ok := g.wsLimit.acquire(r)
	if !ok {
		g.cfg.metrics.RecordWSRejected(reason)
		http.Error(w, "ws limit exceeded: "+reason, http.StatusTooManyRequests)
		return nil, nil, false
	}
	if g.draining.Load() {
		release()
		http.Error(w, "gateway is draining", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(r.Context())
	// The caller-id seam (and any HTTPRequestFromContext consumer)
	// reads inbound headers from the handshake, as on the HTTP path.
	ctx = withHTTPRequest(ctx, r)

	g.wsMu.Lock()
	connKey := uintptr(0)
	for k := uintptr(1); ; k++ {
		if _, exists := g.wsConns[k]; !exists {
			connKey = k
			g.wsConns[k] = cancel
			break
		}
	}
	g.wsMu.Unlock()
	return ctx, func(context.Context) {
		g.wsMu.Lock()
		delete(g.wsConns, connKey)
		g.wsMu.Unlock()
		cancel()
		release()
	}, true
}

func toGatRequest(o *graphqlRequestOptions) gat.Request {
	return gat.Request{
		Query:         o.Query,
		Variables:     o.Variables,
		OperationName: o.OperationName,
		DocumentID:    o.DocumentID,
		Extensions:    o.Extensions,
	}
}

// parseMultipartRequest reads a graphql-multipart-request-spec body:
// files substitute into the variables as *Upload.
func parseMultipartRequest(r *http.Request) (gat.Request, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return gat.Request{}, fmt.Errorf("graphql multipart: %w", err)
	}
	opts, err := parseGraphqlMultipart(r, params["boundary"])
	if err != nil {
		return gat.Request{}, err
	}
	return toGatRequest(opts), nil
}

// parseGraphQLBody reads an application/graphql body: the document.
func parseGraphQLBody(r *http.Request) (gat.Request, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return gat.Request{}, err
	}
	return gat.Request{Query: string(b)}, nil
}

// parseFormRequest reads an application/x-www-form-urlencoded body.
func parseFormRequest(r *http.Request) (gat.Request, error) {
	if err := r.ParseForm(); err != nil {
		return gat.Request{}, err
	}
	if opts := graphqlOptionsFromForm(r.PostForm); opts != nil {
		return toGatRequest(opts), nil
	}
	return gat.Request{}, nil
}
