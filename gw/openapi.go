package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// AddOpenAPIBytes registers an in-memory OpenAPI 3.x spec. Same shape
// as AddOpenAPI but skips the file/HTTP fetch — useful when the
// gateway hosts its own huma-defined routes and self-ingests the
// generated spec at boot.
func (g *Gateway) AddOpenAPIBytes(specBytes []byte, opts ...ServiceOption) error {
	return g.addOpenAPIFromBytes(specBytes, "<inline>", opts...)
}

// AddOpenAPI registers an OpenAPI 3.x specification so its operations
// become GraphQL fields. GET operations land on Query; everything else
// (POST/PUT/PATCH/DELETE) lands on Mutation. Each operation's path,
// query, and body parameters become field arguments; the 200/201
// response schema becomes the field return type.
//
// specSource may be a local file path or an http(s) URL pointing at
// the live spec — huma services typically expose this at
// /openapi.json. The spec is fetched and parsed once at registration;
// changes require a restart (dynamic update is a future follow-up).
//
// Required ServiceOption: gateway.To("http://addr"). Optional As(ns)
// sets the GraphQL namespace prefix; default is the spec's title or
// the URL host.
func (g *Gateway) AddOpenAPI(specSource string, opts ...ServiceOption) error {
	specBytes, err := readOpenAPISpec(specSource)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): %w", specSource, err)
	}
	return g.addOpenAPIFromBytes(specBytes, specSource, opts...)
}

func (g *Gateway) addOpenAPIFromBytes(specBytes []byte, label string, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if sc.conn == nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): missing To(host:port or http url)", label)
	}
	addr, err := openAPIBaseURL(sc.conn)
	if err != nil {
		return err
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): parse: %w", label, err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): validate: %w", label, err)
	}
	ns := sc.namespace
	if ns == "" {
		if doc.Info != nil && doc.Info.Title != "" {
			ns = sanitizeNamespace(doc.Info.Title)
		} else {
			ns = "openapi"
		}
	}
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("gateway: AddOpenAPI: %w", err)
	}
	ver, verN, err := parseVersion(sc.version)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	if g.openAPISources == nil {
		g.openAPISources = map[poolKey]*openAPISource{}
	}
	hash := sha256.Sum256(specBytes)
	httpClient := sc.httpClient
	if httpClient == nil {
		httpClient = g.cfg.openAPIHTTP
	}
	key := poolKey{namespace: ns, version: ver}
	if existing, ok := g.openAPISources[key]; ok {
		// Same hash → idempotent multi-replica add. Different hash →
		// configuration drift; reject.
		if existing.hash != hash {
			return fmt.Errorf("gateway: AddOpenAPI: %s/%s already registered with different spec hash", ns, ver)
		}
		existing.addReplica(&openAPIReplica{
			baseURL:    addr,
			httpClient: httpClient,
		})
		return nil
	}
	src := &openAPISource{
		namespace:      ns,
		version:        ver,
		versionN:       verN,
		doc:            doc,
		hash:           hash,
		forwardHeaders: sc.forwardHeaders,
		rawSpec:        append([]byte(nil), specBytes...),
	}
	if mi := g.cfg.backpressure.MaxInflight; mi > 0 {
		src.sem = make(chan struct{}, mi)
	}
	src.addReplica(&openAPIReplica{
		baseURL:    addr,
		httpClient: httpClient,
	})
	g.openAPISources[key] = src
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// openAPISource is what AddOpenAPI stores. One source per
// (namespace, version), pinned to a single hash. Multiple replicas can
// join a source so long as they all carry the same spec bytes.
// Dispatch picks the lowest-in-flight replica each call (HTTP analogue
// of pool.pickReplica).
type openAPISource struct {
	namespace      string
	version        string // canonical "vN"
	versionN       int    // numeric version for ordering (latest = max)
	doc            *openapi3.T
	hash           [32]byte
	forwardHeaders []string // nil → use defaultForwardedHeaders; empty → forward nothing

	// rawSpec is kept for /schema/openapi re-emit and (cluster mode)
	// for the reconciler to write back into KV when shape changes.
	rawSpec []byte

	// replicas slice replaced (not mutated in place) on add/remove so
	// dispatch closures snapshotting it never see partial mutation.
	// Reads via Load() in pickReplica.
	replicas atomic.Pointer[[]*openAPIReplica]

	// pickHint round-robins between replicas tied at the same lowest
	// in-flight count. Without it, a low-traffic source where every
	// dispatch finishes before the next begins (in-flight always 0)
	// would funnel 100% of traffic to replicas[0]. Atomic-incremented
	// per pickReplica call.
	pickHint atomic.Uint64

	// sem caps simultaneous concurrent dispatches against this source.
	// nil when MaxInflight is 0 (unbounded). Buffered channel; send to
	// acquire, receive to release. HTTP analogue of pool.sem.
	sem chan struct{}

	// queueing tracks waiters on the semaphore for the queue-depth
	// gauge.
	queueing atomic.Int32
}

// openAPIReplica is one HTTP backend behind an openAPISource. Each
// Register call against the same (namespace, hash) appends one of
// these. baseURL is the http(s):// prefix that paths from the spec
// get resolved against; inflight powers pickReplica's least-loaded
// selection.
type openAPIReplica struct {
	id         string       // KV-side replica id; "" for boot-time AddOpenAPI entries
	baseURL    string       // canonical http(s):// prefix, no trailing slash
	owner      string       // registration ID; "" for boot-time
	httpClient *http.Client // nil → fall back to gateway-wide default → http.DefaultClient
	inflight   atomic.Int32
}

// pickReplica returns the replica with the lowest in-flight count,
// breaking ties via round-robin so serial low-traffic dispatch still
// spreads across replicas. Returns nil when the source is empty
// (transient state during drain).
func (s *openAPISource) pickReplica() *openAPIReplica {
	rs := s.replicas.Load()
	if rs == nil || len(*rs) == 0 {
		return nil
	}
	// Find the minimum in-flight value.
	minN := (*rs)[0].inflight.Load()
	for _, r := range (*rs)[1:] {
		if n := r.inflight.Load(); n < minN {
			minN = n
		}
	}
	// Among replicas tied at the minimum, round-robin via pickHint.
	hint := s.pickHint.Add(1) - 1
	n := uint64(len(*rs))
	for i := uint64(0); i < n; i++ {
		r := (*rs)[(hint+i)%n]
		if r.inflight.Load() == minN {
			return r
		}
	}
	// Race: replica counters changed mid-walk. Fall back to first.
	return (*rs)[0]
}

// addReplica appends r. Returns the new replica count.
func (s *openAPISource) addReplica(r *openAPIReplica) int {
	cur := s.replicas.Load()
	var next []*openAPIReplica
	if cur != nil {
		next = make([]*openAPIReplica, 0, len(*cur)+1)
		next = append(next, *cur...)
	}
	next = append(next, r)
	s.replicas.Store(&next)
	return len(next)
}

// removeReplicaByID drops the replica with the given KV id, returning
// the removed *openAPIReplica or nil if not present.
func (s *openAPISource) removeReplicaByID(id string) *openAPIReplica {
	cur := s.replicas.Load()
	if cur == nil || id == "" {
		return nil
	}
	next := make([]*openAPIReplica, 0, len(*cur))
	var removed *openAPIReplica
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
func (s *openAPISource) removeReplicasByOwner(owner string) int {
	cur := s.replicas.Load()
	if cur == nil || owner == "" {
		return 0
	}
	next := make([]*openAPIReplica, 0, len(*cur))
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
func (s *openAPISource) findReplicaByID(id string) *openAPIReplica {
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

func (s *openAPISource) replicaCount() int {
	cur := s.replicas.Load()
	if cur == nil {
		return 0
	}
	return len(*cur)
}

// addOpenAPISourceLocked is the internal hook the control-plane and
// reconciler share. Idempotent under hash equality: a duplicate
// register with matching bytes appends a new replica to the existing
// source (HTTP analogue of joinPoolLocked). Rejects mismatched hash.
// Caller holds g.mu.
//
// replicaID may be empty for boot-time / standalone control-plane
// callers; cluster-driven callers pass the registry KV replica id so
// reconciler.handleDelete can later remove the matching replica.
func (g *Gateway) addOpenAPISourceLocked(ns, ver, baseURL string, specBytes []byte, hash [32]byte, owner, replicaID string) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	canonicalVer, verN, err := parseVersion(ver)
	if err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	ver = canonicalVer
	if g.openAPISources == nil {
		g.openAPISources = map[poolKey]*openAPISource{}
	}
	addr := strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	key := poolKey{namespace: ns, version: ver}
	if existing, ok := g.openAPISources[key]; ok {
		if existing.hash != hash {
			return fmt.Errorf("openapi: %s/%s already registered with different spec hash", ns, ver)
		}
		// Idempotent: if a replica with the same id already lives
		// here, treat as no-op (reconciler replays).
		if replicaID != "" && existing.findReplicaByID(replicaID) != nil {
			return nil
		}
		existing.addReplica(&openAPIReplica{
			id:         replicaID,
			baseURL:    addr,
			owner:      owner,
			httpClient: g.cfg.openAPIHTTP,
		})
		return nil
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return fmt.Errorf("openapi: parse %s/%s: %w", ns, ver, err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		return fmt.Errorf("openapi: validate %s/%s: %w", ns, ver, err)
	}
	src := &openAPISource{
		namespace: ns,
		version:   ver,
		versionN:  verN,
		doc:       doc,
		hash:      hash,
		rawSpec:   append([]byte(nil), specBytes...),
	}
	if mi := g.cfg.backpressure.MaxInflight; mi > 0 {
		src.sem = make(chan struct{}, mi)
	}
	src.addReplica(&openAPIReplica{
		id:         replicaID,
		baseURL:    addr,
		owner:      owner,
		httpClient: g.cfg.openAPIHTTP,
	})
	g.openAPISources[key] = src
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// removeOpenAPIReplicaByIDLocked drops the single replica matching
// (ns, ver, replicaID). When the source's last replica leaves, the
// source itself is deleted and the schema rebuilt. Caller holds g.mu.
func (g *Gateway) removeOpenAPIReplicaByIDLocked(ns, ver, replicaID string) {
	src, ok := g.openAPISources[poolKey{namespace: ns, version: ver}]
	if !ok {
		return
	}
	if src.removeReplicaByID(replicaID) == nil {
		return
	}
	if src.replicaCount() == 0 {
		delete(g.openAPISources, poolKey{namespace: ns, version: ver})
		if g.schema.Load() != nil {
			_ = g.assembleLocked()
		}
	}
}

// removeOpenAPISourcesByOwnerLocked walks every source removing
// replicas whose owner matches. Sources whose last replica leaves
// are deleted. Schema rebuilt once if any source was destroyed.
// Returns the count of replicas removed.
func (g *Gateway) removeOpenAPISourcesByOwnerLocked(owner string) int {
	if owner == "" {
		return 0 // boot-time replicas aren't evictable
	}
	removed := 0
	rebuild := false
	for k, s := range g.openAPISources {
		n := s.removeReplicasByOwner(owner)
		if n == 0 {
			continue
		}
		removed += n
		if s.replicaCount() == 0 {
			delete(g.openAPISources, k)
			rebuild = true
		}
	}
	if rebuild && g.schema.Load() != nil {
		_ = g.assembleLocked()
	}
	return removed
}

// hashOpenAPISpec produces a stable hash for a registered OpenAPI
// spec. v1 just SHA256s the raw bytes — round-tripping through
// kin-openapi to canonicalise paths is a tier-2 follow-up if cluster
// nodes ever fight over hash drift from formatting differences.
func hashOpenAPISpec(specBytes []byte) [32]byte {
	return sha256.Sum256(specBytes)
}

// prepOpenAPIBinding extracts (namespace, hash) from a ServiceBinding
// whose openapi_spec field is set. Defaults the namespace to the
// spec's Info.Title if not provided.
func prepOpenAPIBinding(b interface {
	GetNamespace() string
	GetOpenapiSpec() []byte
}) (string, [32]byte, error) {
	specBytes := b.GetOpenapiSpec()
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("parse openapi spec: %w", err)
	}
	ns := b.GetNamespace()
	if ns == "" {
		if doc.Info != nil && doc.Info.Title != "" {
			ns = sanitizeNamespace(doc.Info.Title)
		} else {
			ns = "openapi"
		}
	}
	if err := validateNS(ns); err != nil {
		return "", [32]byte{}, err
	}
	return ns, hashOpenAPISpec(specBytes), nil
}

// readOpenAPISpec fetches a spec from a URL or reads from disk.
func readOpenAPISpec(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := http.Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("status %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return readFile(src)
}

// openAPIBaseURL extracts the base URL from a ServiceOption. Accepts
// strings of the form "http://host:port" or just "host:port" (sugar
// for http://host:port).
func openAPIBaseURL(c any) (string, error) {
	lc, ok := c.(*lazyConn)
	if !ok {
		return "", fmt.Errorf("AddOpenAPI: To(...) must be a host:port or http(s):// URL string")
	}
	addr := lc.addr
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/"), nil
}

func sanitizeNamespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ', r == '-':
			b.WriteRune('_')
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "openapi"
	}
	return out
}

// ---------------------------------------------------------------------
// Schema assembly: walk operations, add Query/Mutation fields.
// ---------------------------------------------------------------------

// buildOpenAPIFields walks every registered OpenAPI source and builds
// query and mutation fields. Returns (queries, mutations). Conflicting
// field names within the same root error.
//
// Multi-version: sources are grouped by namespace and sorted by
// versionN. The latest version's fields use the unprefixed
// "<ns>_<op>" naming (back-compat with the single-version case). Older
// versions are surfaced as "<ns>_<vN>_<op>" with a GraphQL deprecation
// reason — same "latest is current; older addressable but discouraged"
// shape proto pools use, expressed via name disambiguation rather than
// nested namespace objects.
func (g *Gateway) buildOpenAPIFields(tb *openAPITypeBuilder, filter schemaFilter) (graphql.Fields, graphql.Fields, error) {
	queries := graphql.Fields{}
	mutations := graphql.Fields{}

	// Group sources by namespace, applying the selector filter.
	byNS := map[string][]*openAPISource{}
	for k, s := range g.openAPISources {
		if g.isInternal(k.namespace) {
			continue
		}
		if !filter.matchPool(k) {
			continue
		}
		byNS[k.namespace] = append(byNS[k.namespace], s)
	}

	nsNames := make([]string, 0, len(byNS))
	for ns := range byNS {
		nsNames = append(nsNames, ns)
	}
	sort.Strings(nsNames)

	for _, ns := range nsNames {
		sources := byNS[ns]
		sort.Slice(sources, func(i, j int) bool { return sources[i].versionN < sources[j].versionN })
		latest := sources[len(sources)-1]
		latestReason := fmt.Sprintf("%s is current", latest.version)

		for _, src := range sources {
			isLatest := src.versionN == latest.versionN
			fieldPrefix := ns + "_"
			typePrefix := ns + "_"
			if !isLatest {
				fieldPrefix = ns + "_" + src.version + "_"
				typePrefix = ns + "_" + src.version + "_"
			}
			tb.withPrefix(typePrefix)

			paths := src.doc.Paths
			if paths == nil {
				continue
			}
			pathKeys := make([]string, 0)
			for k := range paths.Map() {
				pathKeys = append(pathKeys, k)
			}
			sort.Strings(pathKeys)
			for _, p := range pathKeys {
				pathItem := paths.Map()[p]
				for _, op := range listOperations(p, pathItem) {
					opKey := openAPIFieldName("", op)
					field, err := g.buildOpenAPIField(tb, src, op, opKey)
					if err != nil {
						return nil, nil, err
					}
					if !isLatest {
						field.DeprecationReason = latestReason
					}
					name := fieldPrefix + opKey
					switch strings.ToUpper(op.Method) {
					case "GET":
						if _, exists := queries[name]; exists {
							return nil, nil, fmt.Errorf("openapi field name collision (Query): %s", name)
						}
						queries[name] = field
					default:
						if _, exists := mutations[name]; exists {
							return nil, nil, fmt.Errorf("openapi field name collision (Mutation): %s", name)
						}
						mutations[name] = field
					}
				}
			}
		}
	}
	return queries, mutations, nil
}

type openAPIOperation struct {
	Method   string
	Path     string
	Op       *openapi3.Operation
	PathItem *openapi3.PathItem
}

func listOperations(path string, item *openapi3.PathItem) []openAPIOperation {
	out := []openAPIOperation{}
	verbs := []struct {
		verb string
		op   *openapi3.Operation
	}{
		{"GET", item.Get},
		{"POST", item.Post},
		{"PUT", item.Put},
		{"PATCH", item.Patch},
		{"DELETE", item.Delete},
	}
	for _, v := range verbs {
		if v.op == nil {
			continue
		}
		out = append(out, openAPIOperation{Method: v.verb, Path: path, Op: v.op, PathItem: item})
	}
	return out
}

// openAPIFieldName builds a GraphQL field name from an operation.
// Prefers operationId (huma sets these); falls back to a method+path
// slug. The caller passes the per-source prefix ("<ns>_" for the
// latest version, "<ns>_<vN>_" for older) so cross-spec / cross-
// version collisions are isolated by name.
func openAPIFieldName(prefix string, op openAPIOperation) string {
	id := op.Op.OperationID
	if id == "" {
		id = strings.ToLower(op.Method) + pathToSlug(op.Path)
	}
	return prefix + lowerCamel(sanitizeNamespace(id))
}

func pathToSlug(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r == '/' || r == '{' || r == '}':
			b.WriteRune('_')
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "_")
}

func (g *Gateway) buildOpenAPIField(tb *openAPITypeBuilder, src *openAPISource, op openAPIOperation, opKey string) (*graphql.Field, error) {
	args := graphql.FieldConfigArgument{}

	// Path + query parameters → args.
	for _, paramRef := range op.Op.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		if p.In != "path" && p.In != "query" {
			continue // skip header/cookie for v1
		}
		t, err := tb.inputTypeFromSchema(p.Schema)
		if err != nil {
			return nil, err
		}
		if p.Required {
			t = graphql.NewNonNull(t)
		}
		args[p.Name] = &graphql.ArgumentConfig{Type: t}
	}

	// Request body → 'body' arg (input object).
	if op.Op.RequestBody != nil && op.Op.RequestBody.Value != nil {
		body := op.Op.RequestBody.Value
		if mt, ok := body.Content["application/json"]; ok && mt.Schema != nil {
			t, err := tb.inputTypeFromSchema(mt.Schema)
			if err != nil {
				return nil, err
			}
			if body.Required {
				t = graphql.NewNonNull(t)
			}
			args["body"] = &graphql.ArgumentConfig{Type: t}
		}
	}

	// Response: prefer 200, then 201, then 'default'.
	out, err := tb.responseType(op.Op)
	if err != nil {
		return nil, err
	}

	core := newOpenAPIDispatcher(src, op.Op, op.Method, op.Path, g.cfg.metrics)
	dispatcher := BackpressureMiddleware(openAPIBackpressureConfig(src, core.label, g.cfg.metrics, g.cfg.backpressure))(core)
	sid := ir.MakeSchemaID(src.namespace, src.version, opKey)
	g.dispatchers.Set(sid, dispatcher)

	return &graphql.Field{
		Type: out,
		Args: args,
		Resolve: func(rp graphql.ResolveParams) (any, error) {
			d := g.dispatchers.Get(sid)
			if d == nil {
				return nil, Reject(CodeInternal, fmt.Sprintf("gateway: no dispatcher for %s", sid))
			}
			return d.Dispatch(rp.Context, rp.Args)
		},
	}, nil
}

// dispatchOpenAPI substitutes path params, encodes query + body, sends
// the HTTP request, and decodes the JSON response. httpClient nil
// means http.DefaultClient.
func dispatchOpenAPI(
	ctx context.Context,
	method, baseURL, pathTemplate string,
	op *openapi3.Operation,
	gqlArgs map[string]any,
	forwardHeaders []string,
	httpClient *http.Client,
) (any, error) {
	resolvedPath := pathTemplate
	queryArgs := url.Values{}
	for _, paramRef := range op.Parameters {
		if paramRef == nil || paramRef.Value == nil {
			continue
		}
		p := paramRef.Value
		v, ok := gqlArgs[p.Name]
		if !ok {
			continue
		}
		strVal := fmt.Sprintf("%v", v)
		switch p.In {
		case "path":
			resolvedPath = strings.ReplaceAll(resolvedPath, "{"+p.Name+"}", url.PathEscape(strVal))
		case "query":
			queryArgs.Add(p.Name, strVal)
		}
	}

	full := baseURL + resolvedPath
	if len(queryArgs) > 0 {
		full += "?" + queryArgs.Encode()
	}

	var body io.Reader
	if bv, ok := gqlArgs["body"]; ok && bv != nil {
		b, err := json.Marshal(bv)
		if err != nil {
			return nil, Reject(CodeInvalidArgument, fmt.Sprintf("openapi: marshal body: %s", err.Error()))
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, Reject(CodeInvalidArgument, fmt.Sprintf("openapi: build request: %s", err.Error()))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	forwardOpenAPIHeaders(ctx, req, forwardHeaders)

	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("openapi: %s %s: %s", method, full, err.Error()))
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("openapi: %s %s: read body: %s", method, full, err.Error()))
	}
	if resp.StatusCode >= 400 {
		return nil, Reject(httpStatusToCode(resp.StatusCode),
			fmt.Sprintf("openapi: %s %s: %s: %s", method, full, resp.Status, strings.TrimSpace(string(respBytes))))
	}
	if len(respBytes) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return nil, Reject(CodeInternal, fmt.Sprintf("openapi: decode response: %s", err.Error()))
	}
	return out, nil
}

// httpStatusToCode maps HTTP status codes onto the gateway's Code enum
// so OpenAPI dispatch errors classify the same way gRPC dispatch
// errors do (gRPC dispatch already maps via google.golang.org/grpc
// status codes in classifyError). Status codes outside the listed
// 4xx specifics fall back to INVALID_ARGUMENT for 4xx and INTERNAL
// for 5xx — same families gRPC's HTTP-mapping conventions use.
func httpStatusToCode(status int) Code {
	switch status {
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusTooManyRequests:
		return CodeResourceExhausted
	}
	if status >= 500 {
		return CodeInternal
	}
	return CodeInvalidArgument
}

// readFile is os.ReadFile in a function var so tests can swap it.
var readFile = os.ReadFile

// defaultForwardedHeaders is the allowlist used when an OpenAPI source
// hasn't called ForwardHeaders. Authorization is the dogfood case
// (admin_* mutations forwarding the bearer to /admin/*). Other auth
// schemes (X-Api-Key, mTLS, service-account tokens) opt in per source.
var defaultForwardedHeaders = []string{"Authorization"}

// forwardOpenAPIHeaders copies the configured allowlist from the
// inbound GraphQL request onto outbound OpenAPI dispatches. allow ==
// nil → use defaultForwardedHeaders. allow == []{} → forward nothing.
func forwardOpenAPIHeaders(ctx context.Context, out *http.Request, allow []string) {
	if allow == nil {
		allow = defaultForwardedHeaders
	}
	if len(allow) == 0 {
		return
	}
	in := HTTPRequestFromContext(ctx)
	if in == nil {
		return
	}
	for _, h := range allow {
		if v := in.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
}

// ---------------------------------------------------------------------
// Type mapper: JSON Schema → GraphQL.
// ---------------------------------------------------------------------

type openAPITypeBuilder struct {
	mu sync.Mutex
	// typePrefix is the per-source prefix (e.g. "petstore_" for the
	// latest version, "petstore_v1_" for older). Object/Enum/Input
	// names are this prefix + schemaName so two versions of the same
	// namespace produce distinct GraphQL types.
	typePrefix string
	objects    map[string]*graphql.Object
	inputs     map[string]*graphql.InputObject
	enums      map[string]*graphql.Enum
	unions     map[string]*graphql.Union
	jsonScalar *graphql.Scalar
	longScalar *graphql.Scalar
}

func newOpenAPITypeBuilder() *openAPITypeBuilder {
	return &openAPITypeBuilder{
		objects: map[string]*graphql.Object{},
		inputs:  map[string]*graphql.InputObject{},
		enums:   map[string]*graphql.Enum{},
		unions:  map[string]*graphql.Union{},
		jsonScalar: graphql.NewScalar(graphql.ScalarConfig{
			Name:         "JSON",
			Description:  "Untyped JSON value (used as a fallback for OpenAPI schemas the gateway can't map exactly).",
			Serialize:    func(v any) any { return v },
			ParseValue:   func(v any) any { return v },
			ParseLiteral: func(v ast.Value) any { return v },
		}),
		longScalar: graphql.NewScalar(graphql.ScalarConfig{
			Name: "Long",
			Description: "64-bit integer encoded as a decimal string. " +
				"OpenAPI integer fields with format=int64/uint64 land here; " +
				"graphql-go's built-in Int is signed 32-bit and would lose " +
				"precision (or null out entirely) for values above 2^31.",
			Serialize: func(v any) any {
				switch x := v.(type) {
				case float64:
					// json.Unmarshal turns numeric responses into float64.
					// strconv.FormatFloat with -1 precision avoids the "%v"
					// scientific notation graphql.String falls into.
					return strconv.FormatInt(int64(x), 10)
				case int64:
					return strconv.FormatInt(x, 10)
				case uint64:
					return strconv.FormatUint(x, 10)
				case int:
					return strconv.Itoa(x)
				case string:
					return x
				case json.Number:
					return x.String()
				}
				return nil
			},
			ParseValue: func(v any) any { return v },
			ParseLiteral: func(v ast.Value) any {
				switch x := v.(type) {
				case *ast.StringValue:
					return x.Value
				case *ast.IntValue:
					return x.Value
				}
				return nil
			},
		}),
	}
}

// withPrefix scopes type-name caching for the next batch of fields.
// Callers set this per-source (latest = "<ns>_", older = "<ns>_<vN>_")
// before invoking outputTypeFromSchema / inputTypeFromSchema, so two
// versions of the same namespace get distinct Object / Enum / Input
// types even when their underlying schemas share a name like "Pet".
func (tb *openAPITypeBuilder) withPrefix(p string) {
	tb.typePrefix = p
}

// primaryType strips "null" from an OpenAPI 3.1 multi-type
// declaration, returning the single non-null type. Returns "" if the
// schema has zero or multiple non-null types (we treat those as
// opaque JSON).
func primaryType(s *openapi3.Schema) string {
	if s == nil || s.Type == nil {
		return ""
	}
	var primaries []string
	for _, t := range *s.Type {
		if t != "null" {
			primaries = append(primaries, t)
		}
	}
	if len(primaries) == 1 {
		return primaries[0]
	}
	return ""
}

// outputTypeFromSchema returns a GraphQL output Type for the given
// OpenAPI schema. Unsupported shapes (allOf, mixed types) fall back
// to the JSON scalar. oneOf / anyOf project to graphql.NewUnion when
// every variant resolves to a known Object; otherwise fall back.
func (tb *openAPITypeBuilder) outputTypeFromSchema(ref *openapi3.SchemaRef) (graphql.Output, error) {
	if ref == nil || ref.Value == nil {
		return tb.jsonScalar, nil
	}
	s := ref.Value
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return tb.unionFromVariants(ref, s)
	}
	if pt := primaryType(s); pt != "" {
		switch pt {
		case "string":
			if len(s.Enum) > 0 {
				return tb.enumFor(ref, s)
			}
			return graphql.String, nil
		case "integer":
			return tb.integerType(s), nil
		case "number":
			return graphql.Float, nil
		case "boolean":
			return graphql.Boolean, nil
		case "array":
			elem, err := tb.outputTypeFromSchema(s.Items)
			if err != nil {
				return nil, err
			}
			return graphql.NewList(elem), nil
		case "object":
			return tb.objectFor(ref, s)
		}
	}
	return tb.jsonScalar, nil
}

// integerType picks a GraphQL output for an OpenAPI `integer` schema
// based on its declared format. graphql-go's `Int` is signed 32-bit,
// so any int64-shaped value (Unix-ms timestamps, large IDs, ...)
// silently coerces to nil and trips a NonNull error on the wire.
// Mirrors gw/types.go's proto-side mapping where Int64Kind also
// becomes a string-encoded representation. Returns the per-builder
// `Long` scalar (decimal-formatted on serialize) for int64 / uint64
// formats; plain integer stays graphql.Int.
func (tb *openAPITypeBuilder) integerType(s *openapi3.Schema) graphql.Output {
	switch s.Format {
	case "int64", "uint64":
		return tb.longScalar
	}
	return graphql.Int
}

// unionFromVariants projects an OpenAPI oneOf / anyOf into a
// graphql.NewUnion when every variant resolves to a known Object.
// Falls back to the JSON scalar when:
//   - a variant doesn't resolve to an Object (primitive / array / nested
//     union / unnamed inline schema we can't synthesize a name for);
//   - the union itself has no stable name (not a $ref, no title, and
//     fewer than two named variants to compose from);
//   - the variant list is empty.
//
// ResolveType uses the spec's `discriminator.propertyName` when present
// (with `discriminator.mapping` overriding the default schema-name
// mapping); falls back to a "first variant whose required props are
// all set on the value" heuristic; returns nil when nothing matches
// (graphql-go surfaces that as a runtime error so the operator notices).
func (tb *openAPITypeBuilder) unionFromVariants(ref *openapi3.SchemaRef, s *openapi3.Schema) (graphql.Output, error) {
	variants := s.OneOf
	if len(variants) == 0 {
		variants = s.AnyOf
	}
	if len(variants) == 0 {
		return tb.jsonScalar, nil
	}

	// Resolve each variant; bail to JSON scalar if any isn't a clean Object.
	type resolvedVariant struct {
		name     string // un-prefixed schema name (the Object's local key)
		obj      *graphql.Object
		schema   *openapi3.Schema
		required []string
	}
	resolved := make([]resolvedVariant, 0, len(variants))
	for _, v := range variants {
		if v == nil || v.Value == nil {
			return tb.jsonScalar, nil
		}
		// Each variant must resolve to an OBJECT.
		if pt := primaryType(v.Value); pt != "" && pt != "object" {
			return tb.jsonScalar, nil
		}
		out, err := tb.objectFor(v, v.Value)
		if err != nil {
			return tb.jsonScalar, nil
		}
		obj, ok := out.(*graphql.Object)
		if !ok {
			return tb.jsonScalar, nil
		}
		resolved = append(resolved, resolvedVariant{
			name:     schemaName(v, v.Value, "Object"),
			obj:      obj,
			schema:   v.Value,
			required: v.Value.Required,
		})
	}

	unionLocalName := schemaName(ref, s, "")
	if unionLocalName == "" {
		// Synthesize a name from the variants — "AOrB". Only safe when
		// every variant has a stable name itself.
		parts := make([]string, len(resolved))
		for i, v := range resolved {
			if v.name == "" {
				return tb.jsonScalar, nil
			}
			parts[i] = v.name
		}
		unionLocalName = strings.Join(parts, "Or")
	}
	name := tb.typePrefix + unionLocalName
	if u, ok := tb.unions[name]; ok {
		return u, nil
	}

	// Discriminator is on s, not on the variant. Build the value→Object
	// lookup once and close over it.
	byVariantName := map[string]*graphql.Object{}
	for _, v := range resolved {
		byVariantName[v.name] = v.obj
	}
	mapping := map[string]*graphql.Object{}
	if s.Discriminator != nil {
		for k, ref := range s.Discriminator.Mapping {
			// Mapping values are typically "#/components/schemas/Foo"
			// — kin-openapi exposes the raw string via .Ref. Plain
			// schema names ("Foo") survive the split unchanged.
			parts := strings.Split(ref.Ref, "/")
			leaf := parts[len(parts)-1]
			if obj, ok := byVariantName[leaf]; ok {
				mapping[k] = obj
			}
		}
	}
	discriminatorProp := ""
	if s.Discriminator != nil {
		discriminatorProp = s.Discriminator.PropertyName
	}

	types := make([]*graphql.Object, len(resolved))
	for i, v := range resolved {
		types[i] = v.obj
	}
	u := graphql.NewUnion(graphql.UnionConfig{
		Name:        name,
		Description: s.Description,
		Types:       types,
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			m, ok := p.Value.(map[string]any)
			if !ok {
				return nil
			}
			if discriminatorProp != "" {
				if d, ok := m[discriminatorProp].(string); ok {
					if obj, ok := mapping[d]; ok {
						return obj
					}
					if obj, ok := byVariantName[d]; ok {
						return obj
					}
				}
			}
			// Heuristic: first variant whose required properties are
			// all present on the value. Falls through to nil when no
			// variant matches — graphql-go surfaces that to the client.
			for _, v := range resolved {
				ok := true
				for _, req := range v.required {
					if _, present := m[req]; !present {
						ok = false
						break
					}
				}
				if ok {
					return v.obj
				}
			}
			return nil
		},
	})
	tb.unions[name] = u
	return u, nil
}

func (tb *openAPITypeBuilder) inputTypeFromSchema(ref *openapi3.SchemaRef) (graphql.Input, error) {
	if ref == nil || ref.Value == nil {
		return tb.jsonScalar, nil
	}
	s := ref.Value
	if len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return tb.jsonScalar, nil
	}
	if pt := primaryType(s); pt != "" {
		switch pt {
		case "string":
			if len(s.Enum) > 0 {
				e, err := tb.enumFor(ref, s)
				if err != nil {
					return nil, err
				}
				return e, nil
			}
			return graphql.String, nil
		case "integer":
			// Same int64-overflow concern as the output path; clients
			// pass large ints through the same Long scalar.
			if s.Format == "int64" || s.Format == "uint64" {
				return tb.longScalar, nil
			}
			return graphql.Int, nil
		case "number":
			return graphql.Float, nil
		case "boolean":
			return graphql.Boolean, nil
		case "array":
			elem, err := tb.inputTypeFromSchema(s.Items)
			if err != nil {
				return nil, err
			}
			return graphql.NewList(elem), nil
		case "object":
			return tb.inputObjectFor(ref, s)
		}
	}
	return tb.jsonScalar, nil
}

func (tb *openAPITypeBuilder) responseType(op *openapi3.Operation) (graphql.Output, error) {
	if op.Responses == nil {
		return tb.jsonScalar, nil
	}
	for _, code := range []string{"200", "201"} {
		r := op.Responses.Status(parseStatus(code))
		if r != nil && r.Value != nil {
			if mt, ok := r.Value.Content["application/json"]; ok && mt.Schema != nil {
				return tb.outputTypeFromSchema(mt.Schema)
			}
		}
	}
	if r := op.Responses.Default(); r != nil && r.Value != nil {
		if mt, ok := r.Value.Content["application/json"]; ok && mt.Schema != nil {
			return tb.outputTypeFromSchema(mt.Schema)
		}
	}
	return tb.jsonScalar, nil
}

func parseStatus(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (tb *openAPITypeBuilder) objectFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (graphql.Output, error) {
	name := tb.typePrefix + schemaName(ref, s, "Object")
	if obj, ok := tb.objects[name]; ok {
		return obj, nil
	}
	// Pre-register an empty Object to handle recursive refs.
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name: name,
		Fields: graphql.FieldsThunk(func() graphql.Fields {
			fields := graphql.Fields{}
			propNames := make([]string, 0, len(s.Properties))
			for k := range s.Properties {
				propNames = append(propNames, k)
			}
			sort.Strings(propNames)
			for _, k := range propNames {
				if !validGraphQLName(k) {
					continue // e.g. $schema (JSON Schema metaschema)
				}
				p := s.Properties[k]
				t, err := tb.outputTypeFromSchema(p)
				if err != nil {
					continue
				}
				if isRequired(s.Required, k) {
					t = graphql.NewNonNull(t)
				}
				fields[lowerCamel(k)] = &graphql.Field{Type: t}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.Field{Type: graphql.String}
			}
			return fields
		}),
	})
	tb.objects[name] = obj
	return obj, nil
}

func (tb *openAPITypeBuilder) inputObjectFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (graphql.Input, error) {
	name := tb.typePrefix + schemaName(ref, s, "Input") + "Input"
	if io, ok := tb.inputs[name]; ok {
		return io, nil
	}
	io := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: name,
		Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
			fields := graphql.InputObjectConfigFieldMap{}
			propNames := make([]string, 0, len(s.Properties))
			for k := range s.Properties {
				propNames = append(propNames, k)
			}
			sort.Strings(propNames)
			for _, k := range propNames {
				if !validGraphQLName(k) {
					continue
				}
				p := s.Properties[k]
				t, err := tb.inputTypeFromSchema(p)
				if err != nil {
					continue
				}
				if isRequired(s.Required, k) {
					t = graphql.NewNonNull(t)
				}
				fields[lowerCamel(k)] = &graphql.InputObjectFieldConfig{Type: t}
			}
			if len(fields) == 0 {
				fields["_void"] = &graphql.InputObjectFieldConfig{Type: graphql.String}
			}
			return fields
		}),
	})
	tb.inputs[name] = io
	return io, nil
}

// validGraphQLName matches /^[_A-Za-z][_A-Za-z0-9]*$/. JSON Schema
// allows things like "$schema" that GraphQL forbids; skip those at
// type-build time.
func validGraphQLName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func (tb *openAPITypeBuilder) enumFor(ref *openapi3.SchemaRef, s *openapi3.Schema) (*graphql.Enum, error) {
	name := tb.typePrefix + schemaName(ref, s, "Enum")
	if e, ok := tb.enums[name]; ok {
		return e, nil
	}
	values := graphql.EnumValueConfigMap{}
	for _, v := range s.Enum {
		vs := fmt.Sprintf("%v", v)
		values[sanitizeNamespace(vs)] = &graphql.EnumValueConfig{Value: vs}
	}
	e := graphql.NewEnum(graphql.EnumConfig{Name: name, Values: values})
	tb.enums[name] = e
	return e, nil
}

func schemaName(ref *openapi3.SchemaRef, s *openapi3.Schema, fallback string) string {
	if ref != nil && ref.Ref != "" {
		// "#/components/schemas/Pet" → "Pet"
		parts := strings.Split(ref.Ref, "/")
		return sanitizeNamespace(parts[len(parts)-1])
	}
	if s != nil && s.Title != "" {
		return sanitizeNamespace(s.Title)
	}
	return fallback
}

func isRequired(req []string, name string) bool {
	for _, r := range req {
		if r == name {
			return true
		}
	}
	return false
}
