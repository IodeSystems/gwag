// Package gateway is a small library for fronting gRPC services with a
// GraphQL surface. The zero-config path takes a list of .proto files and
// gRPC destinations and exposes them as a namespaced GraphQL API; the
// power features are layered on as middleware.
package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
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

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

type Gateway struct {
	mu             sync.Mutex
	pools          map[poolKey]*pool
	internal       map[string]bool // namespaces hidden from the public schema
	transforms     []Transform
	schema         atomic.Pointer[graphql.Schema]
	cfg            *config
	cp             *controlPlane
	peers          *peerTracker
	broker         *subBroker
	openAPISources map[poolKey]*openAPISource
	graphQLSources map[poolKey]*graphQLSource

	// dispatchers holds one ir.Dispatcher per (namespace, version,
	// flat-op-name) populated by the per-format field builders during
	// schema rebuild. Resolver closures fetch by SchemaID instead of
	// capturing the dispatcher pointer, which keeps the closure
	// independent of dispatcher identity — non-GraphQL ingress paths
	// (planned: HTTP/JSON, gRPC) can find the same dispatcher by
	// SchemaID without re-walking pools/sources.
	//
	// Dispatchers are registered fresh on each schema rebuild
	// (assembleLocked → buildSchemaLocked clears + repopulates).
	// Caller holds g.mu during rebuild.
	dispatchers *ir.DispatchRegistry

	// ingressRoutes is the (METHOD, path) → dispatcher table consumed
	// by IngressHandler. Rebuilt on every assembleLocked from the
	// freshly-populated dispatchers map. Atomic-swapped so the
	// handler doesn't take g.mu on the hot path.
	ingressRoutes atomic.Pointer[ingressTable]

	// grpcIngressRoutes is the gRPC analogue of ingressRoutes:
	// "/<svcFullName>/<methodName>" → proto-native chained Handler
	// (skipping canonical-args round trip). Consumed by
	// GRPCUnknownHandler. Rebuilt + atomic-swapped on every
	// assembleLocked.
	grpcIngressRoutes atomic.Pointer[grpcIngressTable]

	// streamGlobalSem caps simultaneous subscription streams across
	// every pool — the gateway-wide MaxStreamsTotal ceiling. nil when
	// disabled (0).
	streamGlobalSem chan struct{}
	streamGlobal    atomic.Int32 // active count
	streamGlobalQ   atomic.Int32 // waiting count

	// draining flips to true on Drain(). Health returns 503; new
	// WebSocket upgrades and new GraphQL queries reject; existing
	// connections receive a `complete` and are then closed.
	draining atomic.Bool

	// wsConns is the registry of live WebSocket subscription
	// connections. Each entry is a cancel func bound to the
	// connection's serveWebSocket lifetime; Drain cancels all to
	// force-close active subscriptions.
	wsMu    sync.Mutex
	wsConns map[uintptr]context.CancelFunc

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
	adminToken   []byte
	adminDataDir string
	openAPIHTTP  *http.Client
}

// SubscriptionAuthOptions configures HMAC verification for incoming
// graphql-ws subscriptions. The auth args (hmac, timestamp, kid) are
// auto-injected on every subscription field's SDL; this controls how
// the gateway verifies them at subscribe time.
type SubscriptionAuthOptions struct {
	// Insecure bypasses HMAC verification entirely. Auth args are
	// accepted (for SDL compatibility) but not checked. Dev/local only.
	// Mutually exclusive with Secret / Secrets.
	Insecure bool

	// Secret is the legacy single shared HMAC key. Tokens minted with
	// no kid (or kid=="") verify against this. Signed payload is
	// "<channel>\n<timestamp_unix>" with HMAC-SHA256, base64-encoded.
	// Either Secret, Secrets, or both may be set; if both supply the
	// empty kid, Secret wins.
	Secret []byte

	// Secrets is the keyed-secret set used for token rotation. Each
	// entry maps a key id (kid) to its HMAC secret. Subscribers send
	// their kid alongside hmac/timestamp; the verifier looks it up
	// here, returning UNKNOWN_KID when absent. Tokens carrying a
	// non-empty kid sign over "<kid>\n<channel>\n<timestamp_unix>"
	// so swapping kid can't replay a token across keys. To rotate:
	// add a new (kid,secret) entry, switch signers to it, drop the
	// old entry once outstanding tokens have aged out.
	Secrets map[string][]byte

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

// BackpressureOptions controls concurrency caps and the wait budget.
//
// Unary dispatches use a per-pool cap (MaxInflight) because each pool
// is one backend service with finite throughput — slow service X
// shouldn't gate dispatches to service Y.
//
// Subscriptions are different: backed by NATS pub/sub, the per-message
// cost on the backend is essentially zero (one NATS sub serves N
// WebSockets via in-process fanout). The scarce resource is the
// gateway itself (file descriptors, RAM, goroutines). MaxStreamsTotal
// caps the gateway as a whole; MaxStreams stays per-pool for operators
// who want fine-grained throttling.
//
// The MaxWaitTime budget applies to all three caps.
type BackpressureOptions struct {
	// MaxInflight is the per-pool ceiling on simultaneous unary
	// dispatches. 0 disables.
	MaxInflight int

	// MaxStreams is the per-pool ceiling on simultaneous active
	// subscription streams. 0 disables. Useful for fine-grained
	// per-channel throttling. Most deployments leave this generous
	// and rely on MaxStreamsTotal for resource protection.
	MaxStreams int

	// MaxStreamsTotal is the gateway-wide ceiling on simultaneous
	// active subscription streams. 0 disables. Sized to the
	// gateway's resource budget — file descriptors, RAM, goroutines.
	// Beyond ~50-100k per node, scale horizontally with more
	// gateways behind a load balancer.
	MaxStreamsTotal int

	// MaxWaitTime is the per-dispatch wait budget: a dispatch that
	// cannot acquire its slot within this window is rejected with
	// Reject(ResourceExhausted, "wait timeout"). Applies to unary
	// and stream slot acquisition (per-pool and gateway-wide). 0
	// disables the timeout.
	MaxWaitTime time.Duration
}

// DefaultBackpressure is what gateway.New uses unless overridden.
//
// 256 unary in-flight per pool is sized for moderate-throughput
// services. 10,000 streams per pool accommodates typical per-channel
// fan-out without micro-throttling. 100,000 streams gateway-wide is
// the per-node ceiling — beyond this, scale horizontally.
var DefaultBackpressure = BackpressureOptions{
	MaxInflight:     256,
	MaxStreams:      10_000,
	MaxStreamsTotal: 100_000,
	MaxWaitTime:     10 * time.Second,
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

// WithAdminToken pins the gateway's admin bearer token to a
// caller-supplied value (raw bytes; AdminTokenHex returns the
// presentation form). If unset, the gateway generates a 32-byte token
// at New — persisted to <adminDataDir>/admin-token if WithAdminDataDir
// was set, otherwise in-memory only.
//
// The token gates non-public /admin/* HTTP routes and, transitively,
// admin_* GraphQL mutations dispatched through them. It does not
// authenticate services calling each other through the gateway.
func WithAdminToken(token []byte) Option {
	return func(cfg *config) { cfg.adminToken = token }
}

// WithAdminDataDir is the directory under which the gateway persists
// (and reloads) its boot admin token. Pairs naturally with the
// JetStream data dir on a clustered gateway, but standalone gateways
// can pass any writable path.
func WithAdminDataDir(dir string) Option {
	return func(cfg *config) { cfg.adminDataDir = dir }
}

// WithOpenAPIClient sets the *http.Client every OpenAPI dispatch
// uses by default. Use this to plug in a transport with mTLS, a
// custom RoundTripper, signed-URL injection, retry policy, or any
// out-of-band auth scheme. Per-source overrides via the
// `OpenAPIClient(...)` ServiceOption beat this default.
//
// When unset, dispatches use http.DefaultClient.
func WithOpenAPIClient(c *http.Client) Option {
	return func(cfg *config) { cfg.openAPIHTTP = c }
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
	if len(cfg.adminToken) == 0 {
		tok, err := loadOrGenerateAdminToken(cfg.adminDataDir)
		if err != nil {
			panic(fmt.Sprintf("gateway: admin token: %v", err))
		}
		cfg.adminToken = tok
	}
	life, cancel := context.WithCancel(context.Background())
	g := &Gateway{
		cfg:         cfg,
		pools:       map[poolKey]*pool{},
		internal:    map[string]bool{},
		wsConns:     map[uintptr]context.CancelFunc{},
		life:        life,
		lifeCancel:  cancel,
		dispatchers: ir.NewDispatchRegistry(),
	}
	if cfg.backpressure.MaxStreamsTotal > 0 {
		g.streamGlobalSem = make(chan struct{}, cfg.backpressure.MaxStreamsTotal)
	}
	return g
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
	namespace      string
	version        string // canonical "vN"; empty → defaults to v1
	conn           grpc.ClientConnInterface
	internal       bool
	forwardHeaders []string
	httpClient     *http.Client

	// maxConcurrency caps simultaneous unary dispatches against this
	// service (pool / openAPISource). 0 → fall back to the gateway-
	// wide default (BackpressureOptions.MaxInflight). Negative is
	// rejected at registration.
	maxConcurrency int

	// maxConcurrencyPerInstance caps simultaneous unary dispatches
	// against any single replica. 0 → unbounded per replica
	// (concurrency limited only by the service-level cap above).
	// Negative is rejected at registration.
	maxConcurrencyPerInstance int
}

// isInternal reports whether ns is hidden from the public GraphQL
// schema. A namespace is internal if either:
//   - the operator passed AsInternal() at registration (recorded in
//     g.internal), or
//   - the namespace starts with "_". This blanket convention covers
//     reserved namespaces like _admin_auth / _events_auth /
//     _admin_events without requiring every caller to remember
//     AsInternal(). Internal namespaces still register in the
//     dispatch registry (so hooks can call them); they just don't
//     surface in the SDL.
//
// Callers must hold g.mu (or be in a path that doesn't race with
// concurrent registrations).
func (g *Gateway) isInternal(ns string) bool {
	if g.internal[ns] {
		return true
	}
	return len(ns) > 0 && ns[0] == '_'
}

// As overrides the default namespace (filename stem) for a registered
// proto. Collisions across registered protos are an error.
func As(namespace string) ServiceOption {
	return func(c *serviceConfig) { c.namespace = namespace }
}

// Version pins the (namespace, version) coordinate the registration
// joins. Accepts "vN" or "N". Empty / unset defaults to "v1". For
// AddProto / AddProtoDescriptor this is informational — proto's own
// version comes from the package; for AddOpenAPI / AddOpenAPIBytes /
// AddGraphQL it identifies the source within a namespace, mirroring
// the proto pool model: latest-flat under the namespace + every
// version addressable as `<ns>.<vN>` with @deprecated on older.
func Version(v string) ServiceOption {
	return func(c *serviceConfig) { c.version = v }
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

// ForwardHeaders sets the per-source allowlist of HTTP headers
// forwarded from the inbound GraphQL request to outbound OpenAPI
// dispatches. Replaces the default ([]string{"Authorization"}) when
// supplied. Pass an empty list to forward nothing.
//
// Currently a no-op for AddProto / AddProtoDescriptor — gRPC dispatch
// uses ctx propagation, not HTTP headers.
func ForwardHeaders(headers ...string) ServiceOption {
	return func(c *serviceConfig) {
		c.forwardHeaders = append([]string(nil), headers...)
	}
}

// MaxConcurrency caps simultaneous unary dispatches against this
// service. Overrides the gateway-wide
// BackpressureOptions.MaxInflight default for this one registration
// — a heavy backend can declare 64 in-flight while light backends
// stay at the gateway default. 0 → fall back to the gateway default.
//
// Applies to AddProto / AddProtoDescriptor (per-pool sem) and
// AddOpenAPI / AddOpenAPIBytes (per-source sem). No-op for
// AddGraphQL (downstream-GraphQL stitching has no per-source sem).
func MaxConcurrency(n int) ServiceOption {
	return func(c *serviceConfig) { c.maxConcurrency = n }
}

// MaxConcurrencyPerInstance caps simultaneous unary dispatches
// against any single replica behind a service. New axis: the
// service-level cap (MaxConcurrency / gateway default) bounds the
// pool, this bounds each replica individually, so a 3-replica
// service with MaxConcurrencyPerInstance(50) holds at most
// 150 in-flight even if MaxConcurrency / the gateway default is
// higher. 0 → unbounded per replica (only the service cap applies).
//
// Same applicability as MaxConcurrency.
func MaxConcurrencyPerInstance(n int) ServiceOption {
	return func(c *serviceConfig) { c.maxConcurrencyPerInstance = n }
}

// OpenAPIClient overrides the *http.Client used for outbound
// dispatches against this OpenAPI source. Beats the gateway-wide
// default set by `WithOpenAPIClient(...)`.
//
// Use this when one backend needs a different transport (mTLS to a
// specific service, a per-source RoundTripper that injects a
// service-account token, etc.). No-op for AddProto /
// AddProtoDescriptor.
func OpenAPIClient(c *http.Client) ServiceOption {
	return func(sc *serviceConfig) { sc.httpClient = c }
}

// AddProtoDescriptor registers a service from a compiled-in
// FileDescriptor (e.g. greeterv1.File_greeter_proto from generated
// bindings). Same shape as AddProto but no disk I/O — useful when
// the gateway hosts its own gRPC service and wants to expose it
// through the GraphQL surface (dogfooding).
//
// Namespace defaults to the proto's file stem; override with As().
func (g *Gateway) AddProtoDescriptor(fd protoreflect.FileDescriptor, opts ...ServiceOption) error {
	sc := &serviceConfig{}
	for _, o := range opts {
		o(sc)
	}
	if sc.conn == nil {
		return fmt.Errorf("gateway: AddProtoDescriptor(%s): missing To(...)", fd.Path())
	}
	ns := sc.namespace
	if ns == "" {
		base := string(fd.Path())
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		ns = strings.TrimSuffix(base, ".proto")
	}
	hash, err := hashFromFileDescriptor(fd)
	if err != nil {
		return fmt.Errorf("gateway: hash %s: %w", fd.Path(), err)
	}
	addr := fmt.Sprintf("descriptor:%s", fd.Path())
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	return g.joinPoolLocked(poolEntry{
		namespace:                 ns,
		version:                   "v1",
		hash:                      hash,
		file:                      fd,
		addr:                      addr,
		conn:                      sc.conn,
		owner:                     "",
		maxConcurrency:            sc.maxConcurrency,
		maxConcurrencyPerInstance: sc.maxConcurrencyPerInstance,
	})
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
		namespace:                 ns,
		version:                   "v1",
		hash:                      hash,
		file:                      fd,
		addr:                      addr,
		conn:                      sc.conn,
		owner:                     "", // boot-time, never evicted
		maxConcurrency:            sc.maxConcurrency,
		maxConcurrencyPerInstance: sc.maxConcurrencyPerInstance,
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

	// maxConcurrency / maxConcurrencyPerInstance are captured on the
	// FIRST registration of a (namespace, version) pool — sizing the
	// service-level sem and the per-replica sem respectively. Later
	// joins must agree on both, like proto hash. 0 means "use the
	// gateway default" (service) or "unbounded" (instance). Negative
	// is rejected upstream.
	maxConcurrency            int
	maxConcurrencyPerInstance int
}

// joinPoolLocked finds or creates the pool for (namespace, version) and
// adds a replica. Pool creation triggers a schema rebuild; replica
// churn within an existing pool does not. Caller must hold g.mu.
func (g *Gateway) joinPoolLocked(e poolEntry) error {
	if e.maxConcurrency < 0 {
		return fmt.Errorf("gateway: pool %s/%s: max_concurrency must be ≥ 0", e.namespace, e.version)
	}
	if e.maxConcurrencyPerInstance < 0 {
		return fmt.Errorf("gateway: pool %s/%s: max_concurrency_per_instance must be ≥ 0", e.namespace, e.version)
	}
	key := poolKey{namespace: e.namespace, version: e.version}
	p, exists := g.pools[key]
	if exists {
		if p.hash != e.hash {
			return fmt.Errorf("gateway: pool %s/%s exists with different proto hash", e.namespace, e.version)
		}
		// Concurrency caps are captured on first-create and frozen for
		// the pool's lifetime — later joins must agree. Mismatched
		// caps mean either a control-plane misconfiguration or a
		// rolling deploy that changed the registration shape; surface
		// loudly so it doesn't silently drift.
		if e.maxConcurrency != p.maxConcurrency || e.maxConcurrencyPerInstance != p.maxConcurrencyPerInstance {
			return fmt.Errorf("gateway: pool %s/%s exists with different concurrency caps (have max=%d/inst=%d, got max=%d/inst=%d)",
				e.namespace, e.version, p.maxConcurrency, p.maxConcurrencyPerInstance, e.maxConcurrency, e.maxConcurrencyPerInstance)
		}
		p.addReplica(g.newReplica(p, e))
		g.publishServiceChange(adminEventsActionRegistered, e.namespace, e.version, e.addr, uint32(p.replicaCount()))
		return nil
	}
	_, n, err := parseVersion(e.version)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	p = &pool{
		key:                       key,
		versionN:                  n,
		file:                      e.file,
		hash:                      e.hash,
		maxConcurrency:            e.maxConcurrency,
		maxConcurrencyPerInstance: e.maxConcurrencyPerInstance,
	}
	// Service-level cap: registration override beats gateway default.
	// 0 → fall back to default; default of 0 → unbounded.
	semSize := e.maxConcurrency
	if semSize == 0 {
		semSize = g.cfg.backpressure.MaxInflight
	}
	if semSize > 0 {
		p.sem = make(chan struct{}, semSize)
	}
	if g.cfg.backpressure.MaxStreams > 0 {
		p.streamSem = make(chan struct{}, g.cfg.backpressure.MaxStreams)
	}
	p.addReplica(g.newReplica(p, e))
	g.pools[key] = p
	g.publishServiceChange(adminEventsActionRegistered, e.namespace, e.version, e.addr, uint32(p.replicaCount()))
	if g.schema.Load() != nil {
		// Pool creation always rebuilds — covers all three cases:
		// namespace appeared, new version under existing namespace, or
		// latest changed (highest versionN moved).
		return g.assembleLocked()
	}
	return nil
}

// newReplica constructs a replica with its per-instance sem sized
// against the pool's MaxConcurrencyPerInstance setting (nil sem when
// unset → unbounded per replica).
func (g *Gateway) newReplica(p *pool, e poolEntry) *replica {
	r := &replica{
		id:    e.replicaID,
		addr:  e.addr,
		owner: e.owner,
		conn:  e.conn,
	}
	if p.maxConcurrencyPerInstance > 0 {
		r.sem = make(chan struct{}, p.maxConcurrencyPerInstance)
	}
	return r
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
	g.publishServiceChange(adminEventsActionDeregistered, ns, ver, r.addr, uint32(p.replicaCount()))
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
		// One ServiceChange per removed pool — replica-level granularity
		// here would spam during cluster reconciliation. Use addr=""
		// because the replica list (and addrs) is already gone.
		g.publishServiceChange(adminEventsActionDeregistered, key.namespace, key.version, "", uint32(p.replicaCount()))
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

// Use appends Transforms to the gateway. Each Transform may carry any
// combination of schema-rewrite rules (Schema) and runtime middleware
// (Runtime); empty halves no-op.
func (g *Gateway) Use(transforms ...Transform) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transforms = append(g.transforms, transforms...)
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

// SchemaHandler returns an http.Handler that exports the GraphQL
// schema as SDL or introspection JSON. Mount it at /schema/graphql
// for the canonical path; SchemaProtoHandler and SchemaOpenAPIHandler
// are siblings:
//
//	GET /schema/graphql                          → SDL (default)
//	GET /schema/graphql?format=json              → introspection JSON
//	GET /schema/graphql?service=ns:ver           → SDL filtered to ns:ver
//	GET /schema/proto?service=ns:ver             → FileDescriptorSet (binary)
//	GET /schema/openapi?service=ns               → re-emit ingested OpenAPI specs
//
// The X-Gateway-Environment header carries the cluster's environment
// label on every response so codegen pipelines can record what they
// grabbed.
//
// Selector grammar (shared across the /schema/* family):
//   - ns        → all versions of ns
//   - ns:vN     → just that version of ns (proto pools only; OpenAPI
//     / downstream-GraphQL sources have no version axis)
//   - missing   → no filter, full schema
//
// Filtered requests build a fresh schema per call (cached g.schema is
// the unfiltered one). Codegen pipelines that always want the whole
// thing pay no extra cost.
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
		selectors, err := parseProtoSelectors(r.URL.Query().Get("service"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var schema *graphql.Schema
		if len(selectors) == 0 {
			schema = g.schema.Load()
			if schema == nil {
				http.Error(w, "schema not assembled", http.StatusServiceUnavailable)
				return
			}
		} else {
			g.mu.Lock()
			built, err := g.buildSchemaLocked(schemaFilter{selectors: selectors})
			g.mu.Unlock()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			schema = built
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

// Transform bundles every reshaping concern that lands per gateway-Use
// call: a list of typed schema rewrites (Schema, e.g. HideType) and a
// runtime middleware (Runtime). Empty halves no-op.
type Transform struct {
	Schema  []SchemaRewrite
	Runtime Middleware
}

// SchemaRewrite mutates IR services in place to reshape the external
// surface — strip fields, flip nullability, etc. Concrete rewrites are
// constructed via HideType and friends; renderers that need rewrite-
// specific data (e.g. the proto FDS exporter pulling out hidden type
// names) type-assert to the concrete struct.
type SchemaRewrite interface {
	apply(svcs []*ir.Service)
}

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

func (r *rejection) Error() string              { return r.Msg }
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
