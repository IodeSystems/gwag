// Package gateway is a small library for fronting gRPC services with a
// GraphQL surface. The zero-config path takes a list of .proto files and
// gRPC destinations and exposes them as a namespaced GraphQL API; the
// power features are layered on as middleware.
package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"iter"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Gateway struct {
	mu       sync.Mutex
	pools    map[poolKey]*pool
	internal map[string]bool // namespaces hidden from the public schema
	pairs    []Pair
	schema   atomic.Pointer[graphql.Schema]
	cfg      *config
	cp       *controlPlane
	peers    *peerTracker

	// life is cancelled by Close to stop background goroutines.
	life       context.Context
	lifeCancel context.CancelFunc
}

type Option func(*config)
type config struct {
	cluster      *Cluster
	tls          *tls.Config
	metrics      Metrics
	backpressure BackpressureOptions
	subAuth      SubscriptionAuthOptions
}

// SubscriptionAuthOptions configures HMAC verification for incoming
// graphql-ws subscriptions. The auth args (hmac, timestamp) are
// auto-injected on every subscription field's SDL; this controls how
// the gateway verifies them at subscribe time.
type SubscriptionAuthOptions struct {
	// Insecure bypasses HMAC verification entirely. Auth args are
	// accepted (for SDL compatibility) but not checked. Dev/local only.
	// Mutually exclusive with Secret.
	Insecure bool

	// Secret is the shared HMAC key. Required when not Insecure. The
	// signed payload is "<channel>\n<timestamp_unix>" with
	// HMAC-SHA256, base64-encoded.
	Secret []byte

	// SkewWindow caps acceptable clock drift between the signer and
	// the gateway. 0 → 5 minutes default.
	SkewWindow time.Duration
}

// WithSubscriptionAuth enables HMAC verification on subscribes.
func WithSubscriptionAuth(o SubscriptionAuthOptions) Option {
	return func(cfg *config) { cfg.subAuth = o }
}

// WithoutSubscriptionAuth marks subscriptions insecure. Equivalent to
// passing WithSubscriptionAuth(SubscriptionAuthOptions{Insecure: true}).
func WithoutSubscriptionAuth() Option {
	return func(cfg *config) { cfg.subAuth = SubscriptionAuthOptions{Insecure: true} }
}

// BackpressureOptions controls per-pool concurrency caps and the
// global wait budget. The model is intentionally per-pool to keep one
// slow service from blocking dispatches that don't depend on it: a
// dispatch waiting on pool X never blocks a dispatch to pool Y. The
// only gateway-wide knob is MaxWaitTime, which bounds how long ANY
// single dispatch will wait for its internal pool slot before
// short-circuiting with backoff.
type BackpressureOptions struct {
	// MaxInflight is the per-pool ceiling on simultaneous dispatches.
	// Once at the ceiling, additional requests for the same pool
	// wait until a slot opens. 0 disables — dispatches go through
	// unbounded.
	MaxInflight int

	// MaxWaitTime is the per-dispatch wait budget: a dispatch that
	// cannot acquire its pool's slot within this window is rejected
	// with Reject(ResourceExhausted, "wait timeout"). This is the
	// "you cannot even get a slot in N seconds" backoff. 0 disables
	// the timeout (wait forever; only the request context cancels).
	MaxWaitTime time.Duration
}

// DefaultBackpressure is what gateway.New uses unless overridden.
// 256 in-flight is sized for moderate-throughput services; 10s is the
// outer wait window before a sustained-overload backoff kicks in.
var DefaultBackpressure = BackpressureOptions{
	MaxInflight: 256,
	MaxWaitTime: 10 * time.Second,
}

// WithBackpressure overrides the default per-pool concurrency caps.
func WithBackpressure(b BackpressureOptions) Option {
	return func(cfg *config) { cfg.backpressure = b }
}

// WithoutBackpressure removes per-pool limits entirely. Dispatches
// proceed without queueing or waiting; useful for tests and dev.
func WithoutBackpressure() Option {
	return func(cfg *config) { cfg.backpressure = BackpressureOptions{} }
}

// WithMetrics swaps the default Prometheus-backed metrics sink for a
// caller-supplied implementation. Pass nil or use WithoutMetrics to
// disable metrics entirely.
func WithMetrics(m Metrics) Option {
	return func(cfg *config) { cfg.metrics = m }
}

// WithoutMetrics disables metrics. /metrics returns 404; dispatches
// still run normally with no per-call instrumentation.
func WithoutMetrics() Option {
	return func(cfg *config) { cfg.metrics = noopMetrics{} }
}

// WithCluster binds the gateway to an embedded NATS cluster. When set,
// the gateway uses JetStream KV for the service registry and peer
// tracking (replacing the in-memory map). Without it, the gateway falls
// back to the single-node in-memory path.
func WithCluster(c *Cluster) Option {
	return func(cfg *config) { cfg.cluster = c }
}

// WithTLS configures mTLS for outbound gRPC dials made by the
// reconciler when it talks to registered services. Pass the same
// config used for the embedded NATS cluster routes (build it with
// LoadMTLSConfig) so one cert covers both surfaces.
func WithTLS(c *tls.Config) Option {
	return func(cfg *config) { cfg.tls = c }
}

func New(opts ...Option) *Gateway {
	cfg := &config{
		backpressure: DefaultBackpressure,
	}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.metrics == nil {
		cfg.metrics = newPrometheusMetrics()
	}
	life, cancel := context.WithCancel(context.Background())
	return &Gateway{
		cfg:        cfg,
		pools:      map[poolKey]*pool{},
		internal:   map[string]bool{},
		life:       life,
		lifeCancel: cancel,
	}
}

// Close stops background goroutines (peer tracker, janitor). Safe to
// call multiple times. Does not close the bound *Cluster — owners of
// the cluster shut it down themselves.
func (g *Gateway) Close() {
	g.mu.Lock()
	tracker := g.peers
	g.peers = nil
	g.mu.Unlock()
	tracker.stop()
	g.lifeCancel()
}

// Cluster returns the bound cluster, or nil if running standalone.
func (g *Gateway) Cluster() *Cluster {
	return g.cfg.cluster
}

type ServiceOption func(*serviceConfig)
type serviceConfig struct {
	namespace string
	conn      grpc.ClientConnInterface
	internal  bool
}

// As overrides the default namespace (filename stem) for a registered
// proto. Collisions across registered protos are an error.
func As(namespace string) ServiceOption {
	return func(c *serviceConfig) { c.namespace = namespace }
}

// To wires the gRPC destination for a registered proto. Accepts either
// a host:port string (sugar for grpc.NewClient with insecure creds, for
// dev) or a caller-managed *grpc.ClientConn.
func To(dest any) ServiceOption {
	return func(c *serviceConfig) {
		switch v := dest.(type) {
		case grpc.ClientConnInterface:
			c.conn = v
		case string:
			c.conn = &lazyConn{addr: v}
		default:
			panic(fmt.Sprintf("gateway.To: unsupported destination type %T", dest))
		}
	}
}

// AsInternal registers the proto in the callable registry but hides its
// services from the external GraphQL surface. Use for infrastructure
// services (auth, policy, lookup) that hooks call but external clients
// should not see.
func AsInternal() ServiceOption {
	return func(c *serviceConfig) { c.internal = true }
}

// AddProto parses a .proto file and registers it as a single replica
// under (namespace, version=v1). Bodies of services are routed to the
// destination set by To(). Namespace defaults to the filename stem;
// override with As().
//
// Safe to call after Handler() — the schema rebuilds and atomically
// replaces the live one, so dynamic add at runtime works the same way
// boot-time registration does.
func (g *Gateway) AddProto(path string, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if sc.conn == nil {
		return fmt.Errorf("gateway: AddProto(%s): missing To(...)", path)
	}
	fd, err := loadProto(path)
	if err != nil {
		return err
	}
	ns := sc.namespace
	if ns == "" {
		ns = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	hash, err := hashFromFileDescriptor(fd)
	if err != nil {
		return fmt.Errorf("gateway: hash %s: %w", path, err)
	}
	addr := fmt.Sprintf("addproto:%s", path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	return g.joinPoolLocked(poolEntry{
		namespace: ns,
		version:   "v1",
		hash:      hash,
		file:      fd,
		addr:      addr,
		conn:      sc.conn,
		owner:     "", // boot-time, never evicted
	})
}

// poolEntry is the input to joinPoolLocked — what a single
// service-binding registration provides.
type poolEntry struct {
	namespace string
	version   string // canonical "vN"
	hash      [32]byte
	file      protoreflect.FileDescriptor
	addr      string
	conn      grpc.ClientConnInterface
	owner     string
	replicaID string // KV-side replica id, "" for boot-time AddProto
}

// joinPoolLocked finds or creates the pool for (namespace, version) and
// adds a replica. Pool creation triggers a schema rebuild; replica
// churn within an existing pool does not. Caller must hold g.mu.
func (g *Gateway) joinPoolLocked(e poolEntry) error {
	key := poolKey{namespace: e.namespace, version: e.version}
	p, exists := g.pools[key]
	if exists {
		if p.hash != e.hash {
			return fmt.Errorf("gateway: pool %s/%s exists with different proto hash", e.namespace, e.version)
		}
		p.addReplica(&replica{id: e.replicaID, addr: e.addr, owner: e.owner, conn: e.conn})
		return nil
	}
	_, n, err := parseVersion(e.version)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	p = &pool{
		key:      key,
		versionN: n,
		file:     e.file,
		hash:     e.hash,
	}
	if g.cfg.backpressure.MaxInflight > 0 {
		p.sem = make(chan struct{}, g.cfg.backpressure.MaxInflight)
	}
	p.addReplica(&replica{id: e.replicaID, addr: e.addr, owner: e.owner, conn: e.conn})
	g.pools[key] = p
	if g.schema.Load() != nil {
		// Pool creation always rebuilds — covers all three cases:
		// namespace appeared, new version under existing namespace, or
		// latest changed (highest versionN moved).
		return g.assembleLocked()
	}
	return nil
}

// removeReplicaByIDLocked finds and drops the single replica matching
// the given (ns,ver,replicaID). If the pool empties, it's deleted.
// Rebuilds the schema if a pool was destroyed. Caller holds g.mu.
// Returns the removed replica (for conn refcount cleanup) or nil if
// not found.
func (g *Gateway) removeReplicaByIDLocked(ns, ver, replicaID string) (*replica, error) {
	key := poolKey{namespace: ns, version: ver}
	p, ok := g.pools[key]
	if !ok {
		return nil, nil
	}
	r := p.removeReplicaByID(replicaID)
	if r == nil {
		return nil, nil
	}
	if p.replicaCount() == 0 {
		delete(g.pools, key)
		if g.schema.Load() != nil {
			return r, g.assembleLocked()
		}
	}
	return r, nil
}

// removeReplicasByOwnerLocked walks all pools removing replicas with
// the given owner. If any pool empties, drop it. Rebuilds the schema
// if any pool was created or destroyed (replica churn within a still-
// populated pool doesn't change the schema).
func (g *Gateway) removeReplicasByOwnerLocked(owner string) (removed int, err error) {
	rebuild := false
	for key, p := range g.pools {
		n := p.removeReplicasByOwner(owner)
		if n == 0 {
			continue
		}
		removed += n
		if p.replicaCount() == 0 {
			delete(g.pools, key)
			rebuild = true
		}
	}
	if rebuild && g.schema.Load() != nil {
		return removed, g.assembleLocked()
	}
	return removed, nil
}

// Use appends middleware to both pipelines. Pair-shaped middleware
// populates both halves; single-shaped middleware fills one and no-ops
// the other.
func (g *Gateway) Use(pairs ...Pair) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pairs = append(g.pairs, pairs...)
}

// Handler returns the http.Handler that serves the GraphQL schema.
// First call assembles the schema and starts hot-swap mode; subsequent
// AddProto / control-plane registrations rebuild the schema in place.
//
// Mount it directly on the GraphQL path (e.g. "/graphql"); pair with
// SchemaHandler at "/schema" if you want a codegen-friendly export.
func (g *Gateway) Handler() http.Handler {
	g.mu.Lock()
	if g.schema.Load() == nil {
		if err := g.assembleLocked(); err != nil {
			g.mu.Unlock()
			return errorHandler(err)
		}
	}
	g.mu.Unlock()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			g.serveWebSocket(w, r)
			return
		}
		schema := g.schema.Load()
		gh := handler.New(&handler.Config{
			Schema:   schema,
			Pretty:   true,
			GraphiQL: true,
		})
		ctx := withInjectCache(r.Context())
		ctx = WithHTTPRequest(ctx, r)
		gh.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SchemaHandler returns an http.Handler that exports the current
// GraphQL schema for client codegen pipelines:
//
//	GET /schema                  → SDL (text/plain; application/graphql)
//	GET /schema?format=json      → introspection result (application/json)
//
// The X-Gateway-Environment header carries the cluster's environment
// label so codegen pipelines can record what they grabbed.
func (g *Gateway) SchemaHandler() http.Handler {
	g.mu.Lock()
	if g.schema.Load() == nil {
		if err := g.assembleLocked(); err != nil {
			g.mu.Unlock()
			return errorHandler(err)
		}
	}
	g.mu.Unlock()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		schema := g.schema.Load()
		if schema == nil {
			http.Error(w, "schema not assembled", http.StatusServiceUnavailable)
			return
		}
		if env := g.environmentLabel(); env != "" {
			w.Header().Set("X-Gateway-Environment", env)
		}
		switch r.URL.Query().Get("format") {
		case "json":
			result := graphql.Do(graphql.Params{Schema: *schema, RequestString: introspectionQuery})
			w.Header().Set("Content-Type", "application/json")
			_ = writeJSON(w, result)
		default:
			w.Header().Set("Content-Type", "application/graphql; charset=utf-8")
			_, _ = w.Write([]byte(printSchemaSDL(schema)))
		}
	})
}

// environmentLabel returns the cluster's environment label, or "" if
// running standalone or no --environment was set.
func (g *Gateway) environmentLabel() string {
	if g.cfg.cluster == nil {
		return ""
	}
	return g.cfg.cluster.Environment
}

type httpRequestCtxKey struct{}

// WithHTTPRequest stores r on ctx so middleware and resolvers can read
// the inbound HTTP request (headers, cookies, remote addr) without
// having to plumb it as a separate argument.
func WithHTTPRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, httpRequestCtxKey{}, r)
}

// HTTPRequestFromContext returns the HTTP request that originated this
// gateway call, or nil if ctx wasn't created by the gateway handler.
func HTTPRequestFromContext(ctx context.Context) *http.Request {
	r, _ := ctx.Value(httpRequestCtxKey{}).(*http.Request)
	return r
}

func errorHandler(err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

// ---------------------------------------------------------------------
// Middleware primitives
// ---------------------------------------------------------------------

type Handler func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error)
type Middleware func(next Handler) Handler

type StreamHandler func(ctx context.Context, in iter.Seq2[protoreflect.ProtoMessage, error]) iter.Seq2[protoreflect.ProtoMessage, error]
type StreamMiddleware func(next StreamHandler) StreamHandler

type Pair struct {
	// Hides lists proto message types that should be stripped from
	// every input position in the external schema. The runtime half is
	// expected to populate any field whose type appears here.
	Hides []protoreflect.FullName

	Schema  SchemaMiddleware
	Runtime Middleware
	Stream  StreamMiddleware
}

// Schema and SchemaMiddleware are reserved for forward use (custom
// schema rewrites that don't fit the Hides model). The current built-in
// case is HideAndInject, which does not need them.
type Schema struct {
	GraphQL *graphql.Schema
}

type SchemaHandler func(*Schema) (*Schema, error)
type SchemaMiddleware func(next SchemaHandler) SchemaHandler

// ---------------------------------------------------------------------
// Reject — short-circuit error
// ---------------------------------------------------------------------

func Reject(code Code, msg string) error {
	return &rejection{Code: code, Msg: msg}
}

type Code int

const (
	CodeUnauthenticated Code = iota + 1
	CodePermissionDenied
	CodeResourceExhausted
	CodeInvalidArgument
	CodeNotFound
	CodeInternal
)

type rejection struct {
	Code Code
	Msg  string
}

func (r *rejection) Error() string         { return r.Msg }
func (r *rejection) Extensions() map[string]any { return map[string]any{"code": r.Code.String()} }

func (c Code) String() string {
	switch c {
	case CodeUnauthenticated:
		return "UNAUTHENTICATED"
	case CodePermissionDenied:
		return "PERMISSION_DENIED"
	case CodeResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case CodeInvalidArgument:
		return "INVALID_ARGUMENT"
	case CodeNotFound:
		return "NOT_FOUND"
	case CodeInternal:
		return "INTERNAL"
	default:
		return "UNKNOWN"
	}
}

// ---------------------------------------------------------------------
// lazyConn defers grpc.NewClient until first use, so To("host:port")
// doesn't error at registration if the destination isn't dialable yet.
// ---------------------------------------------------------------------

type lazyConn struct {
	addr string
	once sync.Once
	conn *grpc.ClientConn
	err  error
}

func (l *lazyConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	c, err := l.dial()
	if err != nil {
		return err
	}
	return c.Invoke(ctx, method, args, reply, opts...)
}

func (l *lazyConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	c, err := l.dial()
	if err != nil {
		return nil, err
	}
	return c.NewStream(ctx, desc, method, opts...)
}

func (l *lazyConn) dial() (*grpc.ClientConn, error) {
	l.once.Do(func() {
		l.conn, l.err = grpc.NewClient(l.addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	})
	return l.conn, l.err
}
