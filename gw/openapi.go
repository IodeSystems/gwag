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
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/IodeSystems/graphql-go"
	"github.com/IodeSystems/graphql-go/language/ast"
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
	if sc.maxConcurrency < 0 {
		return fmt.Errorf("gateway: AddOpenAPI(%s): MaxConcurrency must be ≥ 0", label)
	}
	if sc.maxConcurrencyPerInstance < 0 {
		return fmt.Errorf("gateway: AddOpenAPI(%s): MaxConcurrencyPerInstance must be ≥ 0", label)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	hash := sha256.Sum256(specBytes)
	httpClient := sc.httpClient
	if httpClient == nil {
		httpClient = g.cfg.openAPIHTTP
	}
	key := poolKey{namespace: ns, version: ver}
	existed, err := g.registerSlotLocked(slotKindOpenAPI, key, hash, sc.maxConcurrency, sc.maxConcurrencyPerInstance, nil)
	if err != nil {
		return fmt.Errorf("gateway: AddOpenAPI(%s): %w", label, err)
	}
	s := g.slots[key]
	if existed {
		existing := s.openapi
		existing.addReplica(newOpenAPIReplica(existing, openAPIReplicaInit{
			baseURL:    addr,
			httpClient: httpClient,
		}))
		return nil
	}
	src := &openAPISource{
		namespace:                 ns,
		version:                   ver,
		versionN:                  verN,
		doc:                       doc,
		hash:                      hash,
		forwardHeaders:            sc.forwardHeaders,
		rawSpec:                   append([]byte(nil), specBytes...),
		maxConcurrency:            sc.maxConcurrency,
		maxConcurrencyPerInstance: sc.maxConcurrencyPerInstance,
	}
	semSize := sc.maxConcurrency
	if semSize == 0 {
		semSize = g.cfg.backpressure.MaxInflight
	}
	if semSize > 0 {
		src.sem = make(chan struct{}, semSize)
	}
	src.addReplica(newOpenAPIReplica(src, openAPIReplicaInit{
		baseURL:    addr,
		httpClient: httpClient,
	}))
	s.openapi = src
	g.bakeSlotIRLocked(s)
	g.advanceStableLocked(ns, verN)
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// openAPIReplicaInit is the per-replica init bag used by every
// addReplica call site (boot-time, control-plane standalone,
// reconciler in cluster mode). Mirrors poolEntry's role for proto.
type openAPIReplicaInit struct {
	id         string
	baseURL    string
	owner      string
	httpClient *http.Client
}

// newOpenAPIReplica constructs a replica with its per-instance sem
// sized against the source's MaxConcurrencyPerInstance setting (nil
// sem when unset → unbounded per replica).
func newOpenAPIReplica(src *openAPISource, init openAPIReplicaInit) *openAPIReplica {
	r := &openAPIReplica{
		id:         init.id,
		baseURL:    init.baseURL,
		owner:      init.owner,
		httpClient: init.httpClient,
	}
	if src.maxConcurrencyPerInstance > 0 {
		r.sem = make(chan struct{}, src.maxConcurrencyPerInstance)
	}
	return r
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

	// maxConcurrency / maxConcurrencyPerInstance frozen at first
	// registration; later joins must agree.
	maxConcurrency            int
	maxConcurrencyPerInstance int

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
	// Sized at create time by max(registration's MaxConcurrency,
	// gateway default); nil when both are 0 (unbounded). HTTP analogue
	// of pool.sem.
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

	// sem caps simultaneous concurrent dispatches against this single
	// replica. Sized by source.maxConcurrencyPerInstance; nil when
	// unbounded.
	sem chan struct{}

	// queueing tracks waiters on the per-replica sem.
	queueing atomic.Int32
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
// source (HTTP analogue of joinPoolLocked). Caller holds g.mu.
//
// Tier policy (unstable swap, vN immutability, cross-kind reject) is
// centralized in `registerSlotLocked` — see slot.go.
//
// replicaID may be empty for boot-time / standalone control-plane
// callers; cluster-driven callers pass the registry KV replica id so
// reconciler.handleDelete can later remove the matching replica.
//
// maxConcurrency / maxConcurrencyPerInstance carry the
// ServiceBinding's per-binding caps (0 → gateway default for
// service-level, unbounded per-instance). Frozen at first
// registration (overwritten on unstable swap).
func (g *Gateway) addOpenAPISourceLocked(ns, ver, baseURL string, specBytes []byte, hash [32]byte, owner, replicaID string, maxConcurrency, maxConcurrencyPerInstance int) error {
	if err := validateNS(ns); err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	if maxConcurrency < 0 {
		return fmt.Errorf("openapi: %s/%s: max_concurrency must be ≥ 0", ns, ver)
	}
	if maxConcurrencyPerInstance < 0 {
		return fmt.Errorf("openapi: %s/%s: max_concurrency_per_instance must be ≥ 0", ns, ver)
	}
	canonicalVer, verN, err := parseVersion(ver)
	if err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	ver = canonicalVer
	addr := strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	key := poolKey{namespace: ns, version: ver}
	existed, err := g.registerSlotLocked(slotKindOpenAPI, key, hash, maxConcurrency, maxConcurrencyPerInstance, nil)
	if err != nil {
		return fmt.Errorf("openapi: %w", err)
	}
	s := g.slots[key]
	if existed {
		existing := s.openapi
		// Idempotent: if a replica with the same id already lives
		// here, treat as no-op (reconciler replays).
		if replicaID != "" && existing.findReplicaByID(replicaID) != nil {
			return nil
		}
		existing.addReplica(newOpenAPIReplica(existing, openAPIReplicaInit{
			id:         replicaID,
			baseURL:    addr,
			owner:      owner,
			httpClient: g.cfg.openAPIHTTP,
		}))
		return nil
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(specBytes)
	if err != nil {
		delete(g.slots, key)
		return fmt.Errorf("openapi: parse %s/%s: %w", ns, ver, err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		delete(g.slots, key)
		return fmt.Errorf("openapi: validate %s/%s: %w", ns, ver, err)
	}
	src := &openAPISource{
		namespace:                 ns,
		version:                   ver,
		versionN:                  verN,
		doc:                       doc,
		hash:                      hash,
		rawSpec:                   append([]byte(nil), specBytes...),
		maxConcurrency:            maxConcurrency,
		maxConcurrencyPerInstance: maxConcurrencyPerInstance,
	}
	semSize := maxConcurrency
	if semSize == 0 {
		semSize = g.cfg.backpressure.MaxInflight
	}
	if semSize > 0 {
		src.sem = make(chan struct{}, semSize)
	}
	src.addReplica(newOpenAPIReplica(src, openAPIReplicaInit{
		id:         replicaID,
		baseURL:    addr,
		owner:      owner,
		httpClient: g.cfg.openAPIHTTP,
	}))
	s.openapi = src
	g.bakeSlotIRLocked(s)
	g.advanceStableLocked(ns, verN)
	if g.schema.Load() != nil {
		return g.assembleLocked()
	}
	return nil
}

// removeOpenAPIReplicaByIDLocked drops the single replica matching
// (ns, ver, replicaID). When the source's last replica leaves, the
// source itself is deleted and the schema rebuilt. Caller holds g.mu.
func (g *Gateway) removeOpenAPIReplicaByIDLocked(ns, ver, replicaID string) {
	key := poolKey{namespace: ns, version: ver}
	src := g.openAPISlot(key)
	if src == nil {
		return
	}
	if src.removeReplicaByID(replicaID) == nil {
		return
	}
	if src.replicaCount() == 0 {
		g.releaseSlotLocked(key)
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
	for k, slot := range g.slots {
		if slot.kind != slotKindOpenAPI {
			continue
		}
		s := slot.openapi
		n := s.removeReplicasByOwner(owner)
		if n == 0 {
			continue
		}
		removed += n
		if s.replicaCount() == 0 {
			g.releaseSlotLocked(k)
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

// openAPISharedScalars constructs the Long + JSON scalars once per
// schema build. Per-source IRTypeBuilders share these so the final
// graphql.Schema sees one named instance — graphql-go rejects two
// scalars sharing a Name even when they're equivalently shaped.
func openAPISharedScalars() (*graphql.Scalar, *graphql.Scalar) {
	long := graphql.NewScalar(graphql.ScalarConfig{
		Name: "Long",
		Description: "64-bit integer encoded as a decimal string. " +
			"OpenAPI integer fields with format=int64/uint64 land here; " +
			"graphql-go's built-in Int is signed 32-bit and would lose " +
			"precision (or null out entirely) for values above 2^31.",
		Serialize: func(v any) any {
			switch x := v.(type) {
			case float64:
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
	})
	jsonScalar := graphql.NewScalar(graphql.ScalarConfig{
		Name:         "JSON",
		Description:  "Untyped JSON value (used as a fallback for OpenAPI schemas the gateway can't map exactly).",
		Serialize:    func(v any) any { return v },
		ParseValue:   func(v any) any { return v },
		ParseLiteral: func(v ast.Value) any { return v },
	})
	return long, jsonScalar
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
	headerInjectors []HeaderInjector,
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
	injected, err := applyHeaderInjectors(ctx, headerInjectors)
	if err != nil {
		return nil, err
	}
	for k, v := range injected {
		req.Header.Set(k, v)
	}

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

