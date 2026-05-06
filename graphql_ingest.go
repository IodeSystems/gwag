package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/graphql-go/graphql"
)

// AddGraphQL ingests a remote GraphQL service into the local schema
// via SDL-prefix delegation. The gateway introspects the endpoint at
// boot, mirrors every type with a namespace prefix (e.g. `User` →
// `pets_User`), and registers every Query/Mutation field at the top
// level prefixed the same way (`pets_users`, `pets_user(id: ID!)`,
// etc.). Forwarding resolvers reconstruct the original GraphQL
// operation (selection AST minus the prefix) and POST it to the
// remote.
//
// v1 supports Object/Scalar/Enum/InputObject/List/NonNull. Interface,
// Union, and Subscription types are skipped with a registration-time
// warning (no runtime fallback for those shapes yet).
//
//	gw.AddGraphQL("https://pets-svc/graphql", gateway.As("pets"))
//
// Auth pass-through follows the OpenAPI conventions:
// `Authorization` is forwarded by default; per-source override via
// `ForwardHeaders(...)`. `WithOpenAPIClient(*http.Client)` /
// `OpenAPIClient(c)` set the HTTP client used for both introspection
// and dispatch.
func (g *Gateway) AddGraphQL(endpoint string, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if endpoint == "" {
		return fmt.Errorf("gateway: AddGraphQL: endpoint is required")
	}
	httpClient := sc.httpClient
	if httpClient == nil {
		httpClient = g.cfg.openAPIHTTP
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	intro, err := fetchIntrospection(context.Background(), httpClient, endpoint)
	if err != nil {
		return fmt.Errorf("gateway: AddGraphQL(%s): introspect: %w", endpoint, err)
	}
	ns := sc.namespace
	if ns == "" {
		ns = "graphql"
	}
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("gateway: AddGraphQL: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	if g.graphQLSources == nil {
		g.graphQLSources = map[string]*graphQLSource{}
	}
	if _, exists := g.graphQLSources[ns]; exists {
		return fmt.Errorf("gateway: AddGraphQL: namespace %s already registered", ns)
	}
	g.graphQLSources[ns] = &graphQLSource{
		namespace:      ns,
		endpoint:       endpoint,
		introspection:  intro,
		forwardHeaders: sc.forwardHeaders,
		httpClient:     httpClient,
	}
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// graphQLSource is the gateway-internal handle on a registered
// downstream GraphQL endpoint. Stored on Gateway.graphQLSources keyed
// by namespace.
type graphQLSource struct {
	namespace      string
	endpoint       string
	introspection  *introspectionSchema
	forwardHeaders []string     // nil → defaultForwardedHeaders
	httpClient     *http.Client // nil → http.DefaultClient
}

// graphQLResponse is the wire shape of a remote GraphQL response.
type graphQLResponse struct {
	Data   json.RawMessage   `json:"data,omitempty"`
	Errors []json.RawMessage `json:"errors,omitempty"`
}

// dispatchGraphQL posts a GraphQL operation to the remote endpoint
// and returns the parsed response. forwardHeaders is the per-source
// allowlist (same semantics as forwardOpenAPIHeaders).
func dispatchGraphQL(
	ctx context.Context,
	client *http.Client,
	endpoint, query string,
	variables map[string]any,
	forwardHeaders []string,
) (*graphQLResponse, error) {
	body := map[string]any{"query": query}
	if len(variables) > 0 {
		body["variables"] = variables
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("graphql: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	forwardOpenAPIHeaders(ctx, req, forwardHeaders)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("graphql: %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(respBytes)))
	}
	var out graphQLResponse
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, fmt.Errorf("graphql: decode: %w: %s", err, respBytes)
	}
	return &out, nil
}

// buildGraphQLFields walks every registered downstream GraphQL source
// and emits namespace-prefixed Query / Mutation field maps. Type
// mirroring is done lazily inside graphql.NewObject thunks to handle
// recursive type references without needing topological sort.
func (g *Gateway) buildGraphQLFields() (graphql.Fields, graphql.Fields, error) {
	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	for ns, src := range g.graphQLSources {
		if g.isInternal(ns) {
			continue
		}
		mb := newGraphQLMirror(src)
		q, m, err := mb.build()
		if err != nil {
			return nil, nil, fmt.Errorf("graphql ingest %s: %w", ns, err)
		}
		for name, f := range q {
			if _, exists := queries[name]; exists {
				return nil, nil, fmt.Errorf("graphql ingest %s: Query field %s collides", ns, name)
			}
			queries[name] = f
		}
		for name, f := range m {
			if _, exists := mutations[name]; exists {
				return nil, nil, fmt.Errorf("graphql ingest %s: Mutation field %s collides", ns, name)
			}
			mutations[name] = f
		}
	}
	return queries, mutations, nil
}
