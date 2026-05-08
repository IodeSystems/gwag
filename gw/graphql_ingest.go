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
	"sync"
	"sync/atomic"
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
	ver, verN, err := parseVersion(sc.version)
	if err != nil {
		return fmt.Errorf("gateway: AddGraphQL: %w", err)
	}
	hash := hashIntrospection(rawIntro)

	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	if g.graphQLSources == nil {
		g.graphQLSources = map[poolKey]*graphQLSource{}
	}
	key := poolKey{namespace: ns, version: ver}
	if existing, ok := g.graphQLSources[key]; ok {
		if existing.hash != hash {
			return fmt.Errorf("gateway: AddGraphQL: %s/%s already registered with different schema hash", ns, ver)
		}
		existing.addReplica(&graphQLReplica{
			endpoint:   endpoint,
			httpClient: httpClient,
		})
		return nil
	}
	src := &graphQLSource{
		namespace:        ns,
		version:          ver,
		versionN:         verN,
		introspection:    intro,
		rawIntrospection: rawIntro,
		hash:             hash,
		forwardHeaders:   sc.forwardHeaders,
		metrics:          g.cfg.metrics,
	}
	if mi := g.cfg.backpressure.MaxInflight; mi > 0 {
		src.sem = make(chan struct{}, mi)
	}
	src.addReplica(&graphQLReplica{
		endpoint:   endpoint,
		httpClient: httpClient,
	})
	g.graphQLSources[key] = src
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// addGraphQLSourceLocked is the control-plane / reconciler entry
// point. Idempotent under hash equality: a duplicate register with
// matching introspection bytes appends a new replica to the existing
// source; mismatched hash rejects. Caller holds g.mu.
//
// replicaID may be empty for boot-time / standalone control-plane
// callers; cluster-driven callers pass the registry KV replica id so
// reconciler.handleDelete can later remove the matching replica.
func (g *Gateway) addGraphQLSourceLocked(ns, ver, endpoint string, rawIntro []byte, hash [32]byte, owner, replicaID string) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	if endpoint == "" {
		return fmt.Errorf("graphql: endpoint is required")
	}
	canonicalVer, verN, err := parseVersion(ver)
	if err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	ver = canonicalVer
	if g.graphQLSources == nil {
		g.graphQLSources = map[poolKey]*graphQLSource{}
	}
	httpClient := g.cfg.openAPIHTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	key := poolKey{namespace: ns, version: ver}
	if existing, ok := g.graphQLSources[key]; ok {
		if existing.hash != hash {
			return fmt.Errorf("graphql: %s/%s already registered with different schema hash", ns, ver)
		}
		// Idempotent: replay of the same KV value (same replicaID) is
		// a no-op.
		if replicaID != "" && existing.findReplicaByID(replicaID) != nil {
			return nil
		}
		existing.addReplica(&graphQLReplica{
			id:         replicaID,
			endpoint:   endpoint,
			owner:      owner,
			httpClient: httpClient,
		})
		return nil
	}
	intro, err := parseIntrospectionData(rawIntro)
	if err != nil {
		return fmt.Errorf("graphql: parse introspection %s/%s: %w", ns, ver, err)
	}
	src := &graphQLSource{
		namespace:        ns,
		version:          ver,
		versionN:         verN,
		introspection:    intro,
		rawIntrospection: append([]byte(nil), rawIntro...),
		hash:             hash,
		metrics:          g.cfg.metrics,
	}
	if mi := g.cfg.backpressure.MaxInflight; mi > 0 {
		src.sem = make(chan struct{}, mi)
	}
	src.addReplica(&graphQLReplica{
		id:         replicaID,
		endpoint:   endpoint,
		owner:      owner,
		httpClient: httpClient,
	})
	g.graphQLSources[key] = src
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// removeGraphQLReplicaByIDLocked drops the single replica matching
// (ns, ver, replicaID). When the source's last replica leaves, the
// source itself is deleted and the schema rebuilt. Caller holds g.mu.
func (g *Gateway) removeGraphQLReplicaByIDLocked(ns, ver, replicaID string) {
	src, ok := g.graphQLSources[poolKey{namespace: ns, version: ver}]
	if !ok {
		return
	}
	if src.removeReplicaByID(replicaID) == nil {
		return
	}
	if src.replicaCount() == 0 {
		delete(g.graphQLSources, poolKey{namespace: ns, version: ver})
		if g.schema.Load() != nil {
			_ = g.assembleLocked()
		}
	}
}

// removeGraphQLSourcesByOwnerLocked walks every source removing
// replicas whose owner matches. Sources whose last replica leaves
// are deleted. Schema rebuilt once if any source was destroyed.
// Returns the count of replicas removed.
func (g *Gateway) removeGraphQLSourcesByOwnerLocked(owner string) int {
	if owner == "" {
		return 0 // boot-time replicas aren't evictable
	}
	removed := 0
	rebuild := false
	for k, s := range g.graphQLSources {
		n := s.removeReplicasByOwner(owner)
		if n == 0 {
			continue
		}
		removed += n
		if s.replicaCount() == 0 {
			delete(g.graphQLSources, k)
			rebuild = true
		}
	}
	if rebuild && g.schema.Load() != nil {
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
// by (namespace, version). Same hash → identical schema; multiple
// replicas can share a source (mirror of openAPISource). Dispatch
// picks the lowest-in-flight replica per call.
type graphQLSource struct {
	namespace      string
	version        string // canonical "vN"
	versionN       int    // numeric version for ordering (latest = max)
	introspection  *introspectionSchema
	forwardHeaders []string // nil → defaultForwardedHeaders

	// rawIntrospection is the JSON the receiving gateway fetched (and,
	// in cluster mode, cached in the registry KV). Kept for cluster
	// roundtrip and any future /schema/graphql-ingest re-emit.
	rawIntrospection []byte

	// hash is sha256(rawIntrospection); the idempotency key used by
	// addGraphQLSourceLocked.
	hash [32]byte

	// replicas slice replaced (not mutated in place) on add/remove so
	// dispatch closures snapshotting it never see partial mutation.
	// Reads via Load() in pickReplica.
	replicas atomic.Pointer[[]*graphQLReplica]

	// pickHint round-robins between replicas tied at the same lowest
	// in-flight count — without it, a low-traffic source where every
	// dispatch finishes before the next begins would funnel 100% of
	// traffic to replicas[0]. Atomic-incremented per pickReplica call.
	pickHint atomic.Uint64

	// sem caps simultaneous concurrent dispatches against this source.
	// nil when MaxInflight is 0 (unbounded). Buffered channel; send to
	// acquire, receive to release. Mirrors openAPISource.sem so HTTP
	// and downstream-GraphQL dispatch share the same backpressure
	// surface.
	sem chan struct{}

	// queueing tracks waiters on the semaphore for the queue-depth
	// gauge.
	queueing atomic.Int32

	// subBroker is the lazy-init upstream graphql-transport-ws
	// multiplexer. Same (query, vars) across N local subscribers
	// share an upstream subscription via this broker; one upstream
	// WS connection serves the whole namespace. Created on first
	// subscribingResolver call; closed when the last consumer leaves.
	subBrokerOnce sync.Once
	subBroker     *graphQLSubBroker

	// metrics is the gateway's metrics sink, plumbed in so the
	// subBroker can surface per-namespace fanout open/close +
	// active-count without reaching back through the Gateway.
	metrics Metrics
}

// getSubBroker returns the source's subscription multiplexer,
// creating it on first use.
func (s *graphQLSource) getSubBroker() *graphQLSubBroker {
	s.subBrokerOnce.Do(func() {
		s.subBroker = newGraphQLSubBroker(s)
	})
	return s.subBroker
}

// graphQLReplica is one upstream behind a graphQLSource. Each Register
// call against the same (namespace, hash) appends one of these.
// Dispatch picks the lowest-inflight replica each call.
type graphQLReplica struct {
	id         string       // KV-side replica id; "" for boot-time AddGraphQL entries
	endpoint   string       // upstream GraphQL URL (no trailing slash needed)
	owner      string       // registration ID; "" for boot-time
	httpClient *http.Client // nil → http.DefaultClient
	inflight   atomic.Int32
}

// pickReplica returns the replica with the lowest in-flight count,
// breaking ties via round-robin so serial low-traffic dispatch still
// spreads across replicas. Returns nil when the source is empty
// (transient state during drain).
func (s *graphQLSource) pickReplica() *graphQLReplica {
	rs := s.replicas.Load()
	if rs == nil || len(*rs) == 0 {
		return nil
	}
	minN := (*rs)[0].inflight.Load()
	for _, r := range (*rs)[1:] {
		if n := r.inflight.Load(); n < minN {
			minN = n
		}
	}
	hint := s.pickHint.Add(1) - 1
	n := uint64(len(*rs))
	for i := uint64(0); i < n; i++ {
		r := (*rs)[(hint+i)%n]
		if r.inflight.Load() == minN {
			return r
		}
	}
	return (*rs)[0]
}

// addReplica appends r. Returns the new replica count.
func (s *graphQLSource) addReplica(r *graphQLReplica) int {
	cur := s.replicas.Load()
	var next []*graphQLReplica
	if cur != nil {
		next = make([]*graphQLReplica, 0, len(*cur)+1)
		next = append(next, *cur...)
	}
	next = append(next, r)
	s.replicas.Store(&next)
	return len(next)
}

// removeReplicaByID drops the replica with the given KV id, returning
// the removed *graphQLReplica or nil if not present.
func (s *graphQLSource) removeReplicaByID(id string) *graphQLReplica {
	cur := s.replicas.Load()
	if cur == nil || id == "" {
		return nil
	}
	next := make([]*graphQLReplica, 0, len(*cur))
	var removed *graphQLReplica
	for _, r := range *cur {
		if removed == nil && r.id == id {
			removed = r
			continue
		}
		next = append(next, r)
	}
	if removed == nil {
		return nil
	}
	s.replicas.Store(&next)
	return removed
}

// removeReplicasByOwner returns the count removed.
func (s *graphQLSource) removeReplicasByOwner(owner string) int {
	cur := s.replicas.Load()
	if cur == nil || owner == "" {
		return 0
	}
	next := make([]*graphQLReplica, 0, len(*cur))
	removed := 0
	for _, r := range *cur {
		if r.owner == owner {
			removed++
			continue
		}
		next = append(next, r)
	}
	if removed == 0 {
		return 0
	}
	s.replicas.Store(&next)
	return removed
}

// findReplicaByID returns the replica with the given id, or nil.
func (s *graphQLSource) findReplicaByID(id string) *graphQLReplica {
	cur := s.replicas.Load()
	if cur == nil || id == "" {
		return nil
	}
	for _, r := range *cur {
		if r.id == id {
			return r
		}
	}
	return nil
}

func (s *graphQLSource) replicaCount() int {
	cur := s.replicas.Load()
	if cur == nil {
		return 0
	}
	return len(*cur)
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

