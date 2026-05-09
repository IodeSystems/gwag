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
	mu sync.Mutex

	// slots is the §4 single-occupant index per (namespace, version).
	// Every registration goes through `registerSlotLocked` first to
	// enforce the tier policy (unstable swap / vN immutability /
	// cross-kind reject); the per-kind map below it (pools /
	// openAPISources / graphQLSources) holds the dispatch state.
	// Phase 2 of the slot-registry refactor (plan §4) pre-distills
	// `*ir.Service` onto the slot and drops the per-kind maps so
	// schema rebuild can iterate slots directly.
	slots map[poolKey]*slot

	internal   map[string]bool // namespaces hidden from the public schema
	transforms []Transform
	schema     atomic.Pointer[graphql.Schema]

	// stableVN tracks the highest-ever-seen "vN" per namespace.
	// Monotonic; only advances. Read at schema-build time so the
	// renderer can emit a `stable` sub-field aliasing the matching
	// vN's content. See `gw/stable.go`. Cluster-wide convergence is
	// reconciler-driven today; KV-persisted recovery for fresh nodes
	// joining mid-life is a plan §4 follow-up.
	stableVN map[string]int

	// stats is the in-process rolling stats registry — 1m / 1h / 24h
	// per (namespace, version, method) windowed call counts +
	// throughput + p50/p95 latency. The admin UI reads it directly
	// (no Prometheus dependency); Prometheus stays canonical for
	// long-window history. See gw/stats.go.
	stats *statsRegistry

	// deprecation is the side-state mirror of the cluster
	// `go-api-gateway-deprecated` KV bucket. The watch loop
	// populates it; registerSlotLocked reads it to stamp newly-
	// joining slots that already have a deprecation set (covers the
	// reconciler-arrives-after-watch race). Plan §5.
	deprecation map[poolKey]string
	cfg        *config
	cp         *controlPlane
	peers      *peerTracker
	broker     *subBroker

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

	// warnedEventsAuth tracks (namespace, version) tuples we've already
	// emitted a deprecation warning for, so heartbeat-driven re-registers
	// don't spam logs. Keyed on "<ns>:<ver>". The SubscriptionAuthorizer
	// delegate path under _events_auth/v1 is being collapsed in favor of
	// the signer-as-API model — see plan §2.
	warnedEventsAuth sync.Map
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
	signerSecret []byte
	openAPIHTTP  *http.Client

	// allowedTiers gates which version tiers the gateway accepts at
	// registration and renders in the schema. nil → default policy
	// (all three: unstable / stable / vN). See WithAllowTier.
	allowedTiers *AllowedTiers

	// callerHeaders is the inbound HTTP-header allowlist consulted to
	// derive a caller-id label per dispatch (plan §5). Empty → every
	// dispatch gets caller="unknown". The caller string lands as a
	// dimension on the in-process stats registry and as a Prometheus
	// label on go_api_gateway_dispatch_duration_seconds; operators
	// own the cardinality by choosing bounded headers
	// (X-Caller-Service) or accept the User-Agent blast radius
	// explicitly.
	callerHeaders []string
}

// AllowedTiers expresses which §4 version tiers a gateway will accept
// at registration time and surface in the rendered schema. The zero
// value rejects everything; use WithAllowTier (or default — i.e. no
// option) to populate it.
//
//   - Unstable: registrations with version="unstable" accepted.
//   - VN: registrations with version="v<N>" accepted.
//   - Stable: the schema-render `<ns>.stable` alias is emitted. Stable
//     itself is not a registerable version (it's a computed alias),
//     so this gates rendering only.
type AllowedTiers struct {
	Unstable bool
	Stable   bool
	VN       bool
}

// describe returns a human-readable summary of the allow set, for
// rejection error messages. Order matches the canonical order tiers
// are listed in the plan and the --allow-tier flag.
func (t AllowedTiers) describe() string {
	parts := make([]string, 0, 3)
	if t.Unstable {
		parts = append(parts, "unstable")
	}
	if t.Stable {
		parts = append(parts, "stable")
	}
	if t.VN {
		parts = append(parts, "vN")
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ",")
}

// WithCallerHeaders configures the inbound HTTP-header allowlist used
// to derive a caller-id label on every dispatch. Headers are checked
// in order; the first non-empty value wins. When no header matches —
// or no list is configured — caller defaults to "unknown".
//
// Cardinality is operator-controlled:
//   - X-Caller-Service is bounded by your service registry → safe.
//   - User-Agent is unbounded for public-internet traffic → only safe
//     behind authenticated ingress where caller = service name.
//
// Plan §5: the caller string lands as a dimension on the in-process
// stats registry (admin UI consumes it) and as a Prometheus label on
// go_api_gateway_dispatch_duration_seconds.
func WithCallerHeaders(headers ...string) Option {
	return func(cfg *config) {
		cfg.callerHeaders = append([]string(nil), headers...)
	}
}

// WithAllowTier restricts the version tiers this gateway accepts at
// registration and renders in the schema. Plan §4: production
// deployments restrict to "stable","vN" or "vN" alone; dev gateways
// accept all three.
//
// Pass any combination of "unstable", "stable", "vN". Unknown values
// are silently ignored — operators control the spelling at the boot
// flag boundary (`--allow-tier`). Calling with no arguments yields a
// gateway that rejects every registration; calling with all three
// matches the default-when-unset behavior.
//
// Effect on registration:
//   - "unstable" missing: registrations with version="unstable" are
//     rejected with a clear error.
//   - "vN" missing: registrations with numbered versions are rejected.
//
// Effect on schema render:
//   - "stable" missing: the `<ns>.stable` alias is omitted even when
//     a `vN` cut is registered. Callers wanting evergreen-without-
//     recodegen must move to a numbered cut.
//
// "stable" itself is never a registerable version — `parseVersion`
// rejects it. The flag controls only whether the alias surfaces.
func WithAllowTier(tiers ...string) Option {
	return func(cfg *config) {
		t := AllowedTiers{}
		for _, s := range tiers {
			switch s {
			case "unstable":
				t.Unstable = true
			case "stable":
				t.Stable = true
			case "vN":
				t.VN = true
			}
		}
		cfg.allowedTiers = &t
	}
}

// effectiveAllowedTiers returns the tier policy in force. nil-on-cfg
// means the operator never called WithAllowTier; the default is
// permissive (all three).
func (g *Gateway) effectiveAllowedTiers() AllowedTiers {
	if g.cfg.allowedTiers == nil {
		return AllowedTiers{Unstable: true, Stable: true, VN: true}
	}
	return *g.cfg.allowedTiers
}

// checkVersionTierAllowed returns an error if `version` (canonical
// post-parseVersion form: "unstable" or "v<N>") is not in the
// gateway's --allow-tier policy. Plan §4 boot gate: the single hot
// path every registration crosses (registerSlotLocked) calls this
// before allocating a slot, and the control plane Register RPC
// double-checks before writing to the registry KV in cluster mode.
func (g *Gateway) checkVersionTierAllowed(version string) error {
	t := g.effectiveAllowedTiers()
	if version == "unstable" {
		if !t.Unstable {
			return fmt.Errorf("tier %q is not in --allow-tier policy (allowed: %s)", "unstable", t.describe())
		}
		return nil
	}
	if !t.VN {
		return fmt.Errorf("tier %q is not in --allow-tier policy (allowed: %s)", "vN", t.describe())
	}
	return nil
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

// WithSignerSecret installs a sign-specific bearer for the gRPC
// SignSubscriptionToken RPC. When set, remote callers must present
// it in `authorization: Bearer <hex>` metadata; the admin/boot token
// is the always-works fallback. When unset, gRPC SignSubscriptionToken
// requires the admin token only. In-process callers (no gRPC peer in
// ctx, e.g. the huma /admin/sign handler or library embedders calling
// cp.SignSubscriptionToken directly) bypass the gate — they're past
// the trust boundary.
//
// Pass raw bytes; the operator-presented form is hex.
func WithSignerSecret(secret []byte) Option {
	return func(cfg *config) { cfg.signerSecret = secret }
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
	if pm, ok := cfg.metrics.(*prometheusMetrics); ok {
		pm.callerHeaders = cfg.callerHeaders
	}
	stats := newStatsRegistry()
	cfg.metrics = &statsRecordingMetrics{Metrics: cfg.metrics, stats: stats, callerHeaders: cfg.callerHeaders}
	g := &Gateway{
		cfg:         cfg,
		slots:       map[poolKey]*slot{},
		internal:    map[string]bool{},
		wsConns:     map[uintptr]context.CancelFunc{},
		life:        life,
		lifeCancel:  cancel,
		dispatchers: ir.NewDispatchRegistry(),
		stats:       stats,
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
	ver, _, err := parseVersion(sc.version)
	if err != nil {
		return fmt.Errorf("gateway: AddProtoDescriptor(%s): %w", fd.Path(), err)
	}
	addr := fmt.Sprintf("descriptor:%s", fd.Path())
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	return g.joinPoolLocked(poolEntry{
		namespace:                 ns,
		version:                   ver,
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
	ver, _, err := parseVersion(sc.version)
	if err != nil {
		return fmt.Errorf("gateway: AddProto(%s): %w", path, err)
	}
	addr := fmt.Sprintf("addproto:%s", path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc.internal {
		g.internal[ns] = true
	}
	return g.joinPoolLocked(poolEntry{
		namespace:                 ns,
		version:                   ver,
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
//
// Tier policy (unstable swap, vN immutability, cross-kind reject)
// is centralized in `registerSlotLocked` — see slot.go.
func (g *Gateway) joinPoolLocked(e poolEntry) error {
	if e.maxConcurrency < 0 {
		return fmt.Errorf("gateway: pool %s/%s: max_concurrency must be ≥ 0", e.namespace, e.version)
	}
	if e.maxConcurrencyPerInstance < 0 {
		return fmt.Errorf("gateway: pool %s/%s: max_concurrency_per_instance must be ≥ 0", e.namespace, e.version)
	}
	g.warnSubscribeDelegateDeprecated(e.namespace, e.version)
	key := poolKey{namespace: e.namespace, version: e.version}
	existed, err := g.registerSlotLocked(slotKindProto, key, e.hash, e.maxConcurrency, e.maxConcurrencyPerInstance)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	s := g.slots[key]
	if existed {
		p := s.proto
		p.addReplica(g.newReplica(p, e))
		g.publishServiceChange(adminEventsActionRegistered, e.namespace, e.version, e.addr, uint32(p.replicaCount()))
		return nil
	}
	_, n, err := parseVersion(e.version)
	if err != nil {
		// parseVersion already accepted this version on its first run
		// (registerSlotLocked uses the same key) — defensive: roll the
		// slot back so the gateway state stays in sync.
		delete(g.slots, key)
		return fmt.Errorf("gateway: %w", err)
	}
	p := &pool{
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
	s.proto = p
	g.bakeSlotIRLocked(s)
	g.advanceStableLocked(e.namespace, n)
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
	p := g.protoSlot(key)
	if p == nil {
		return nil, nil
	}
	r := p.removeReplicaByID(replicaID)
	if r == nil {
		return nil, nil
	}
	g.publishServiceChange(adminEventsActionDeregistered, ns, ver, r.addr, uint32(p.replicaCount()))
	if p.replicaCount() == 0 {
		g.releaseSlotLocked(key)
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
	for key, s := range g.slots {
		if s.kind != slotKindProto {
			continue
		}
		p := s.proto
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
			g.releaseSlotLocked(key)
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
//
// Schema-half rewrites (HideType / HidePath / NullableType /
// NullablePath — the IR-mutating injectors) are baked into each
// slot's cached IR at registration time, so adding new ones via
// Use(...) requires re-baking every existing slot. Runtime-half
// middleware and header injectors are captured into dispatcher
// closures at the next schema rebuild — those track via the same
// path they did before.
func (g *Gateway) Use(transforms ...Transform) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transforms = append(g.transforms, transforms...)
	// Re-bake every slot so the new schema-half rewrites take effect
	// in slot.ir without waiting for a register/dereg event. Use(...)
	// itself does NOT trigger a schema rebuild — the existing
	// "transforms apply on next rebuild" semantics are preserved;
	// what changes is that slot.ir already reflects the new injectors
	// when the next rebuild reads it.
	g.rebakeAllSlotsLocked()
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
// call: a list of typed schema rewrites (Schema, e.g. HideType), a
// runtime middleware (Runtime), and a list of outbound header
// injectors (Headers, e.g. InjectHeader). Empty halves no-op.
//
// Inventory records the high-level injector(s) this Transform
// represents, captured at construction so the admin inventory endpoint
// can surface what the operator registered without re-deriving it from
// the SchemaRewrite / Headers shapes.
type Transform struct {
	Schema    []SchemaRewrite
	Runtime   Middleware
	Headers   []HeaderInjector
	Inventory []InjectorRecord
}

// HeaderInjector stamps one outbound header (HTTP, OpenAPI dispatch)
// or gRPC metadata key (proto dispatch) on every dispatch the gateway
// sends. Constructed via InjectHeader. ForwardHeaders' inbound
// allowlist is unaffected — header injection writes through directly.
//
// Hide(true): Fn always sees current=nil. Hide(false): Fn sees the
// inbound HTTP request's value for Name (nil when absent or when the
// request isn't HTTP). Returning "" skips the header for this
// dispatch.
type HeaderInjector struct {
	Name string
	Hide bool
	Fn   func(ctx context.Context, current *string) (string, error)
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
