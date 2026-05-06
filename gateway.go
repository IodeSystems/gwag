// Package gateway is a small library for fronting gRPC services with a
// GraphQL surface. The zero-config path takes a list of .proto files and
// gRPC destinations and exposes them as a namespaced GraphQL API; the
// power features are layered on as middleware.
package gateway

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

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
	cluster *Cluster
}

// WithCluster binds the gateway to an embedded NATS cluster. When set,
// the gateway uses JetStream KV for the service registry and peer
// tracking (replacing the in-memory map). Without it, the gateway falls
// back to the single-node in-memory path.
func WithCluster(c *Cluster) Option {
	return func(cfg *config) { cfg.cluster = c }
}

func New(opts ...Option) *Gateway {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
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
		p.addReplica(&replica{addr: e.addr, owner: e.owner, conn: e.conn})
		return nil
	}
	_, n, err := parseVersion(e.version)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	prevLatest := g.latestVersionLocked(e.namespace)
	p = &pool{
		key:      key,
		versionN: n,
		file:     e.file,
		hash:     e.hash,
	}
	p.addReplica(&replica{addr: e.addr, owner: e.owner, conn: e.conn})
	g.pools[key] = p
	if g.schema.Load() != nil {
		// Schema must rebuild: namespace appeared, OR a new version
		// was introduced under an existing namespace, OR latest changed.
		_ = prevLatest
		return g.assembleLocked()
	}
	return nil
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

// latestVersionLocked returns the highest versionN currently live for
// the given namespace, or -1 if none.
func (g *Gateway) latestVersionLocked(ns string) int {
	best := -1
	for k, p := range g.pools {
		if k.namespace != ns {
			continue
		}
		if p.versionN > best {
			best = p.versionN
		}
	}
	return best
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
