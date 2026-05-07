package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

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

	rawIntro, err := fetchIntrospectionBytes(context.Background(), httpClient, endpoint)
	if err != nil {
		return fmt.Errorf("gateway: AddGraphQL(%s): introspect: %w", endpoint, err)
	}
	intro, err := parseIntrospectionData(rawIntro)
	if err != nil {
		return fmt.Errorf("gateway: AddGraphQL(%s): parse introspection: %w", endpoint, err)
	}
	ns := sc.namespace
	if ns == "" {
		ns = "graphql"
	}
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("gateway: AddGraphQL: %w", err)
	}
	hash := hashIntrospection(rawIntro)

	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	return g.addGraphQLSourceLockedDirect(&graphQLSource{
		namespace:        ns,
		endpoint:         endpoint,
		introspection:    intro,
		rawIntrospection: rawIntro,
		hash:             hash,
		forwardHeaders:   sc.forwardHeaders,
		httpClient:       httpClient,
	})
}

// addGraphQLSourceLockedDirect installs `src` under its namespace.
// Idempotent under hash equality (matches the OpenAPI source path):
// re-adding the same ns with the same hash is a no-op; mismatched
// hash is an error. Caller holds g.mu.
func (g *Gateway) addGraphQLSourceLockedDirect(src *graphQLSource) error {
	if g.graphQLSources == nil {
		g.graphQLSources = map[string]*graphQLSource{}
	}
	if existing, ok := g.graphQLSources[src.namespace]; ok {
		if existing.hash != src.hash {
			return fmt.Errorf("gateway: AddGraphQL: namespace %s already registered with different schema hash", src.namespace)
		}
		return nil
	}
	if mi := g.cfg.backpressure.MaxInflight; mi > 0 && src.sem == nil {
		src.sem = make(chan struct{}, mi)
	}
	g.graphQLSources[src.namespace] = src
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// addGraphQLSourceLocked is the control-plane / reconciler entry
// point. Builds a graphQLSource from raw introspection bytes the
// receiving gateway already fetched (and cached in the registry KV)
// and installs it. Idempotent under hash equality.
func (g *Gateway) addGraphQLSourceLocked(ns, endpoint string, rawIntro []byte, hash [32]byte, owner string) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	if endpoint == "" {
		return fmt.Errorf("graphql: endpoint is required")
	}
	if existing, ok := g.graphQLSources[ns]; ok {
		if existing.hash != hash {
			return fmt.Errorf("graphql: namespace %s already registered with different schema hash", ns)
		}
		return nil
	}
	intro, err := parseIntrospectionData(rawIntro)
	if err != nil {
		return fmt.Errorf("graphql: parse introspection %s: %w", ns, err)
	}
	httpClient := g.cfg.openAPIHTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return g.addGraphQLSourceLockedDirect(&graphQLSource{
		namespace:        ns,
		endpoint:         endpoint,
		introspection:    intro,
		rawIntrospection: append([]byte(nil), rawIntro...),
		hash:             hash,
		owner:            owner,
		httpClient:       httpClient,
	})
}

// removeGraphQLSourceLocked drops the source for ns entirely.
// No-op when absent. Caller holds g.mu.
func (g *Gateway) removeGraphQLSourceLocked(ns string) {
	if _, ok := g.graphQLSources[ns]; !ok {
		return
	}
	delete(g.graphQLSources, ns)
	if g.schema.Load() != nil {
		_ = g.assembleLocked()
	}
}

// removeGraphQLSourcesByOwnerLocked drops every source whose owner
// matches. Used by the standalone Deregister path. Returns the count
// removed. Schema rebuilt once if any source was destroyed.
func (g *Gateway) removeGraphQLSourcesByOwnerLocked(owner string) int {
	if owner == "" {
		return 0 // boot-time sources aren't evictable
	}
	removed := 0
	for ns, s := range g.graphQLSources {
		if s.owner != owner {
			continue
		}
		delete(g.graphQLSources, ns)
		removed++
	}
	if removed > 0 && g.schema.Load() != nil {
		_ = g.assembleLocked()
	}
	return removed
}

// hashIntrospection produces a stable hash for a raw introspection
// JSON. Same approach as hashOpenAPISpec — SHA256 of the bytes;
// canonicalisation is a tier-2 follow-up if cluster hash drift from
// formatting differences turns up.
func hashIntrospection(b []byte) [32]byte {
	return sha256.Sum256(b)
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

	// rawIntrospection is the JSON the receiving gateway fetched (and,
	// in cluster mode, cached in the registry KV). Kept for cluster
	// roundtrip and any future /schema/graphql-ingest re-emit.
	rawIntrospection []byte

	// hash is sha256(rawIntrospection); the idempotency key used by
	// addGraphQLSourceLocked.
	hash [32]byte

	// owner is the registration ID for control-plane sources; "" for
	// boot-time AddGraphQL sources (which aren't evictable by owner).
	owner string

	// sem caps simultaneous concurrent dispatches against this source.
	// nil when MaxInflight is 0 (unbounded). Buffered channel; send to
	// acquire, receive to release. Mirrors openAPISource.sem so HTTP
	// and downstream-GraphQL dispatch share the same backpressure
	// surface.
	sem chan struct{}

	// queueing tracks waiters on the semaphore for the queue-depth
	// gauge.
	queueing atomic.Int32
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
		return nil, Reject(CodeInvalidArgument, fmt.Sprintf("graphql: marshal: %s", err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, Reject(CodeInvalidArgument, fmt.Sprintf("graphql: build request: %s", err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	forwardOpenAPIHeaders(ctx, req, forwardHeaders)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("graphql: %s: %s", endpoint, err.Error()))
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("graphql: %s: read body: %s", endpoint, err.Error()))
	}
	if resp.StatusCode >= 400 {
		return nil, Reject(httpStatusToCode(resp.StatusCode),
			fmt.Sprintf("graphql: %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(respBytes))))
	}
	var out graphQLResponse
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("graphql: decode: %s: %s", err.Error(), respBytes))
	}
	return &out, nil
}

// buildGraphQLFields walks every registered downstream GraphQL source
// and emits namespace-prefixed Query / Mutation field maps. Type
// mirroring is done lazily inside graphql.NewObject thunks to handle
// recursive type references without needing topological sort.
func (g *Gateway) buildGraphQLFields(filter schemaFilter) (graphql.Fields, graphql.Fields, error) {
	queries := graphql.Fields{}
	mutations := graphql.Fields{}
	for ns, src := range g.graphQLSources {
		if g.isInternal(ns) {
			continue
		}
		if !filter.matchNS(ns) {
			continue
		}
		mb := newGraphQLMirror(src, g.cfg.metrics, g.cfg.backpressure)
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
