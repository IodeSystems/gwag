// Package controlclient is the convenience layer subservices use to
// self-register with a gwag control plane: one Register call,
// a background heartbeat goroutine, and graceful Deregister on Close.
//
//	//go:embed greeter.proto
//	var greeterProto []byte
//
//	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
//	    GatewayAddr: "gateway:50090",
//	    ServiceAddr: "greeter:50051",
//	    Services: []controlclient.Service{
//	        {Namespace: "greeter", ProtoSource: greeterProto},
//	    },
//	})
//	if err != nil { log.Fatal(err) }
//	defer reg.Close(context.Background())
//
// Holds an open *grpc.ClientConn to the gateway for the lifetime of
// the registration. Re-Register is automatic on heartbeat-not-ok
// (gateway evicted us, e.g. after a restart, or a cluster node that
// never had the registration) — the goroutine fires a fresh Register
// and resumes. With several gateway addresses (Options.GatewayAddrs)
// a heartbeat that cannot reach its gateway moves the registration to
// the next address.
package controlclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Service describes one service binding to register with the gateway.
// At least one of ProtoSource/ProtoFS, OpenAPISpec, or GraphQLEndpoint
// must be set. Multiple Services can be passed in a single SelfRegister
// call to bind multiple namespaces in one go.
//
// Stability: stable
type Service struct {
	// Namespace under which to mount this service in the gateway's
	// GraphQL surface. Empty falls back to the proto's filename stem
	// (proto bindings) or the OpenAPI spec's Info.Title (OpenAPI
	// bindings).
	Namespace string

	// Version, e.g. "v1", "v2". Multiple versions of the same namespace
	// coexist on the gateway; latest surfaces flat under the namespace,
	// older versions appear as `vN` sub-objects with @deprecated.
	// Empty defaults to "v1". Ignored for OpenAPI bindings (single
	// version per ns in v1).
	Version string

	// ProtoSource is the raw bytes of the entrypoint .proto file —
	// exactly what the operator wrote on disk. The receiving gateway
	// compiles via protocompile (SourceInfoStandard) so leading /
	// trailing comments survive into the GraphQL SDL and MCP search
	// corpus. Most callers `//go:embed greeter.proto` and pass the
	// embedded bytes here.
	//
	// Mutually exclusive with ProtoFS, OpenAPISpec, GraphQLEndpoint.
	ProtoSource []byte

	// ProtoImports passes transitive .proto imports keyed by their
	// import path (e.g. "auth.proto"). Required only when ProtoSource
	// has `import "..."` statements; well-known imports
	// (google/protobuf/*) resolve automatically. Single-file .protos
	// leave this nil.
	ProtoImports map[string][]byte

	// ProtoFS is the multi-file ergonomic shape: pass any fs.FS
	// (embed.FS, os.DirFS(...), tar/zip wrappers, anything that
	// satisfies the interface) and the controlclient walks it to
	// build ProtoSource (= bytes of ProtoEntry) and ProtoImports
	// (= every other .proto under the FS, keyed by relative path).
	// Use this when the service has many .proto files; use
	// ProtoSource directly for single-file services.
	//
	// Mutually exclusive with ProtoSource / ProtoImports.
	ProtoFS fs.FS

	// ProtoEntry is the entrypoint filename within ProtoFS (e.g.
	// "user.proto"). Required when ProtoFS is set.
	ProtoEntry string

	// OpenAPISpec is the raw bytes of an OpenAPI 3.x document (JSON
	// or YAML; kin-openapi parses either). When set, ServiceAddr in
	// the parent Options is the HTTP base URL the gateway dispatches
	// to (e.g. "https://billing.internal").
	OpenAPISpec []byte

	// GraphQLEndpoint is the URL of an upstream GraphQL service to
	// ingest under this Namespace. Mutually exclusive with the proto
	// source fields and OpenAPISpec. The receiving gateway runs the
	// canonical introspection query against this endpoint at Register
	// time and forwards dispatches back to it. ServiceAddr is ignored
	// for GraphQL bindings — the endpoint URL carries both the
	// schema source and the dispatch destination.
	GraphQLEndpoint string

	// MaxConcurrency caps simultaneous unary dispatches against this
	// (namespace, version) on the receiving gateway. Overrides the
	// gateway-wide BackpressureOptions.MaxInflight default for this
	// one binding. 0 → fall back to the gateway default.
	MaxConcurrency uint32

	// MaxConcurrencyPerInstance caps simultaneous unary dispatches
	// against any single replica behind this binding. New axis: the
	// service-level cap above bounds the pool, this bounds each
	// replica individually. 0 → unbounded per replica (only the
	// service-level cap applies).
	MaxConcurrencyPerInstance uint32

	// Depends lists the services this binding calls through the
	// gateway, each with the lines it ships clients for. Part of the
	// service manifest; the gateway stores it and reports it in
	// ListServices / /admin/services. Optional.
	Depends []Dependency

	// Owner names the team that owns this binding. Part of the
	// service manifest. Optional.
	Owner *Owner

	// Public shows this binding to admin readers that present no
	// token: the dependency graph, status page and services list.
	// Admin visibility only; calls through the gateway are not
	// affected. Part of the service manifest. Default false.
	Public bool

	// Subscribes lists the event channels this binding listens on,
	// each with its consumer group. Part of the service manifest: the
	// gateway counts each one like a dependency on the service that
	// declares the channel (start gate, sunset rule, dependency
	// graph). Generated event code reports these. Optional.
	Subscribes []Subscription

	// HealthURL and SpecURL turn on verification: the gateway routes
	// to this instance only after its address is reachable, HealthURL
	// answers 200 and the spec served at SpecURL equals the one
	// registered here; and it re-checks periodically. Each is a path
	// ("/healthz") on ServiceAddr, or an absolute http(s) URL on
	// ServiceAddr's host (another port is fine). SpecURL is ignored
	// for GraphQL bindings (the gateway introspects again). Empty =
	// not verified. LoadProfile fills both from the profile.
	HealthURL string
	SpecURL   string
}

// Subscription names one event channel by its key ("<proto
// package>.<channel name>", e.g. "orders.v1.OrderEvents") and the
// consumer group the binding listens as.
//
// Stability: experimental
type Subscription struct {
	Channel string
	Group   string
}

// Dependency names one provider service (its gateway namespace) and
// the lines the caller accepts, e.g. {Service: "library", Lines:
// []string{"v1", "v2"}}. Lines must be "vN" or "unstable"; at least
// one is required.
//
// Stability: stable
type Dependency struct {
	Service string
	Lines   []string
}

// Owner is the team responsible for a service binding and how to
// reach it (email, chat channel, URL — free-form).
//
// Stability: stable
type Owner struct {
	Team    string
	Contact string
}

// manifestProto converts the Service's manifest fields to the wire
// shape.
func (s Service) manifestProto() ([]*cpv1.Dependency, *cpv1.Owner) {
	var deps []*cpv1.Dependency
	for _, d := range s.Depends {
		deps = append(deps, &cpv1.Dependency{
			Service: d.Service,
			Lines:   append([]string(nil), d.Lines...),
		})
	}
	var owner *cpv1.Owner
	if s.Owner != nil {
		owner = &cpv1.Owner{Team: s.Owner.Team, Contact: s.Owner.Contact}
	}
	return deps, owner
}

func (s Service) subscribesProto() []*cpv1.Subscription {
	var out []*cpv1.Subscription
	for _, sub := range s.Subscribes {
		out = append(out, &cpv1.Subscription{Channel: sub.Channel, Group: sub.Group})
	}
	return out
}

// Options configures a SelfRegister call. A gateway address
// (GatewayAddr or GatewayAddrs), ServiceAddr, and at least one Service
// entry are required.
//
// Stability: stable
type Options struct {
	// GatewayAddr is the gRPC host:port where the gateway's control
	// plane is listening (the WithControlPlane addr): one cluster
	// node, or a load balancer in front of the cluster's control
	// planes.
	GatewayAddr string

	// GatewayAddrs are more control-plane addresses of the same
	// cluster, tried after GatewayAddr (which may be empty) in order.
	// The registration lives on one gateway at a time. When a
	// heartbeat cannot reach it (an error, or no answer within
	// min(HeartbeatInterval, 5s)), the Registration registers again
	// on the next address that accepts. The new registration has a
	// new ID; the old one expires with the gateway that held it.
	// SelfRegister also moves to the next address while one is
	// unreachable.
	GatewayAddrs []string

	// ServiceAddr is the host:port where this service's gRPC server
	// is reachable. Passed to the gateway verbatim.
	ServiceAddr string

	// Services to register. All bind to ServiceAddr. Empty rejected.
	Services []Service

	// InstanceID is echoed in ListRegistrations for operator debugging.
	// Free-form ("greeter@pod-abc"). Optional.
	InstanceID string

	// TTL is how long the gateway should retain the registration
	// without seeing a heartbeat. 0 → server default (30s).
	TTL time.Duration

	// HeartbeatInterval defaults to TTL/3 (or 10s if TTL is 0).
	HeartbeatInterval time.Duration

	// ReadyPollInterval is how often SelfRegister polls Heartbeat
	// while a binding is WAITING on its dependencies. 0 → 1s.
	ReadyPollInterval time.Duration

	// DialOptions are appended to the gateway-conn dial. Insecure is
	// the default if none are supplied.
	DialOptions []grpc.DialOption

	// AppKey is sent on every control-plane call as gRPC metadata
	// `authorization: Bearer <AppKey>`. Needed when the gateway runs
	// with WithRequireAppKeys (--require-app-keys); the key must cover
	// every Service's namespace. Optional otherwise.
	AppKey string

	// AppKeyFunc, when set, is called before every control-plane call
	// and its result is sent instead of AppKey: a service that reads
	// its key from a file can pick up a rotated key without a
	// restart. An error fails that call.
	AppKeyFunc func(ctx context.Context) (string, error)

	// Logger is called with non-fatal events (eviction recovery,
	// heartbeat errors). Defaults to log.Printf-equivalent.
	Logger func(format string, args ...any)

	// BuildTag, when non-empty, marks the calling binary as a release
	// build. SelfRegister refuses any Service whose Version is
	// "unstable" — release artifacts belong to numbered cuts (vN), not
	// trunk's mutable slot. Plan §4 forcing function: a redeployed
	// v3-era pod can't accidentally overwrite `unstable` because its
	// release binary still carries v3's tag.
	//
	// Recommended pattern: stamp via -ldflags "-X 'main.buildTag=v1.2.3'"
	// (or whatever the project's release machinery emits) and pass it
	// through to controlclient.Options. Trunk CI omits it; release CI
	// sets it. Empty defeats the lint, so don't paper over a CI bug
	// by clearing the field — unset means "trunk", set means "release".
	BuildTag string

	// DrainOnSIGTERM makes SelfRegister catch SIGTERM. On the signal
	// the Registration asks the gateway to drain it (Drain), waits for
	// safe_to_stop (at most DrainTimeout), deregisters (Close), then
	// calls OnDrained, or when OnDrained is nil re-raises SIGTERM with
	// the default handler so the process exits as it would have.
	//
	// Off by default: catching SIGTERM changes the process's signal
	// handling. A service with its own SIGTERM handler leaves this off
	// and calls Drain from that handler instead.
	DrainOnSIGTERM bool

	// DrainTimeout bounds the wait for safe_to_stop on SIGTERM. 0 →
	// 60s. On timeout the Registration deregisters anyway.
	DrainTimeout time.Duration

	// DrainPollInterval is how often Drain polls the gateway. 0 →
	// 500ms.
	DrainPollInterval time.Duration

	// OnDrained runs after the SIGTERM drain and deregister. nil →
	// re-raise SIGTERM.
	OnDrained func()
}

// Registration is the live handle returned by SelfRegister. Close
// stops the heartbeat goroutine and gracefully deregisters.
//
// Stability: stable
type Registration struct {
	// gws holds one conn per gateway address, in Options order.
	gws []*gatewayConn

	mu     sync.Mutex
	cur    int // index into gws of the gateway that holds id
	id     string
	ttl    time.Duration
	stop   chan struct{}
	done   chan struct{}
	logger func(string, ...any)
	opts   Options

	closeOnce sync.Once
	closeErr  error
	sigStop   chan struct{}

	// bindings is the start-gate state from the latest Register or
	// Heartbeat response (gate.go). Guarded by mu.
	bindings []BindingStatus
}

// SelfRegister registers with the gateway control plane and starts a
// background heartbeat goroutine. Returns a Registration; call
// Close when the service shuts down to deregister and stop the goroutine.
//
// Blocks until the gateway reports every binding ready, or ctx ends:
//
//   - While the gateway is unreachable (Unavailable and similar), the
//     Register is retried with backoff, forever, and each failure is
//     logged. The service does not start without the gateway (fail
//     closed). A refused Register (bad request, dependency cycle)
//     returns its error.
//   - A binding whose Depends have no ready instance on any accepted
//     line is WAITING. SelfRegister polls Heartbeat every
//     ReadyPollInterval and logs which dependencies are missing until
//     the gateway reports it ready.
//
// Bindings without Depends are ready at once. When ctx ends first,
// the registration is deregistered and ctx's error returned. Use
// PresentLines / Bindings to pick which line of a dependency to call.
//
// Stability: stable
func SelfRegister(ctx context.Context, opts Options) (*Registration, error) {
	addrs := gatewayAddrs(opts)
	if len(addrs) == 0 {
		return nil, errors.New("controlclient: GatewayAddr or GatewayAddrs required")
	}
	if opts.ServiceAddr == "" {
		return nil, errors.New("controlclient: ServiceAddr required")
	}
	if len(opts.Services) == 0 {
		return nil, errors.New("controlclient: at least one Service required")
	}
	if opts.BuildTag != "" {
		for _, s := range opts.Services {
			if s.Version == "unstable" {
				return nil, fmt.Errorf("controlclient: BuildTag=%q set; refusing to register %s/unstable (release builds must claim a numbered vN — see plan §4)", opts.BuildTag, s.Namespace)
			}
		}
	}
	if opts.Logger == nil {
		opts.Logger = log.Printf
	}

	dialOpts := opts.DialOptions
	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	if opts.AppKey != "" || opts.AppKeyFunc != nil {
		dialOpts = append(append([]grpc.DialOption(nil), dialOpts...), grpc.WithChainUnaryInterceptor(appKeyInterceptor(opts)))
	}
	r := &Registration{
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logger: opts.Logger,
		opts:   opts,
	}
	for _, addr := range addrs {
		conn, err := grpc.NewClient(addr, dialOpts...)
		if err != nil {
			_ = r.closeConns()
			return nil, fmt.Errorf("dial gateway %s: %w", addr, err)
		}
		r.gws = append(r.gws, &gatewayConn{addr: addr, conn: conn, client: cpv1.NewControlPlaneClient(conn)})
	}

	if err := r.registerRetry(ctx); err != nil {
		_ = r.closeConns()
		return nil, err
	}
	if err := r.waitReady(ctx); err != nil {
		r.deregister()
		_ = r.closeConns()
		return nil, err
	}

	hi := opts.HeartbeatInterval
	if hi == 0 {
		if r.ttl > 0 {
			hi = r.ttl / 3
		} else {
			hi = 10 * time.Second
		}
	}
	go r.heartbeatLoop(hi)
	if opts.DrainOnSIGTERM {
		r.watchSIGTERM()
	}

	return r, nil
}

// appKeyInterceptor adds `authorization: Bearer <key>` to each call.
func appKeyInterceptor(opts Options) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, callOpts ...grpc.CallOption) error {
		key := opts.AppKey
		if opts.AppKeyFunc != nil {
			k, err := opts.AppKeyFunc(ctx)
			if err != nil {
				return fmt.Errorf("controlclient: app key: %w", err)
			}
			key = k
		}
		if key != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
		}
		return invoker(ctx, method, req, reply, cc, callOpts...)
	}
}

// watchSIGTERM runs the DrainOnSIGTERM path.
func (r *Registration) watchSIGTERM() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	r.sigStop = make(chan struct{})
	go func() {
		defer signal.Stop(ch)
		select {
		case <-r.sigStop:
			return
		case <-ch:
		}
		timeout := r.opts.DrainTimeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := r.Drain(ctx); err != nil {
			r.logger("controlclient: drain on SIGTERM: %v; deregistering anyway", err)
		}
		cancel()
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = r.Close(cctx)
		ccancel()
		if r.opts.OnDrained != nil {
			r.opts.OnDrained()
			return
		}
		signal.Stop(ch)
		signal.Reset(syscall.SIGTERM)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
}

// Drain asks the gateway to drain this registration: stop routing new
// requests to it, wait for requests in flight to finish, and hold
// while a dependent would be left without a covering instance. Drain
// polls until the gateway reports safe_to_stop, or returns ctx's
// error with the last state. The registration stays registered (and
// heartbeating); call Close after Drain to deregister.
//
// Stability: stable
func (r *Registration) Drain(ctx context.Context) error {
	interval := r.opts.DrainPollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	var last *cpv1.DrainInstanceResponse
	logged := cpv1.DrainState_DRAIN_STATE_UNSPECIFIED
	for {
		r.mu.Lock()
		id := r.id
		r.mu.Unlock()
		resp, err := r.gateway().client.DrainInstance(ctx, &cpv1.DrainInstanceRequest{Instance: id})
		switch {
		case err != nil && ctx.Err() == nil:
			r.logger("controlclient: drain: %v", err)
		case err == nil:
			last = resp
			if resp.GetState() == cpv1.DrainState_DRAIN_STATE_SAFE_TO_STOP {
				return nil
			}
			if resp.GetState() != logged {
				logged = resp.GetState()
				r.logger("controlclient: drain: %s inflight=%d %s", resp.GetState(), resp.GetInflight(), strings.Join(resp.GetReasons(), "; "))
			}
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("drain: %w (state %s, reasons %v)", ctx.Err(), last.GetState(), last.GetReasons())
			}
			return fmt.Errorf("drain: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

func (r *Registration) register(ctx context.Context) error {
	req := &cpv1.RegisterRequest{
		Addr:       r.opts.ServiceAddr,
		InstanceId: r.opts.InstanceID,
		TtlSeconds: uint32(r.opts.TTL / time.Second),
	}
	for _, s := range r.opts.Services {
		protoSource := s.ProtoSource
		protoImports := s.ProtoImports
		if s.ProtoFS != nil {
			if s.ProtoEntry == "" {
				return fmt.Errorf("service %q: ProtoFS set without ProtoEntry", s.Namespace)
			}
			if len(s.ProtoSource) > 0 || len(s.ProtoImports) > 0 {
				return fmt.Errorf("service %q: ProtoFS is mutually exclusive with ProtoSource / ProtoImports", s.Namespace)
			}
			src, imports, err := resolveProtoFS(s.ProtoFS, s.ProtoEntry)
			if err != nil {
				return fmt.Errorf("service %q: ProtoFS: %w", s.Namespace, err)
			}
			protoSource = src
			protoImports = imports
		}

		hasProto := len(protoSource) > 0
		hasOpenAPI := len(s.OpenAPISpec) > 0
		hasGraphQL := s.GraphQLEndpoint != ""
		set := 0
		if hasProto {
			set++
		}
		if hasOpenAPI {
			set++
		}
		if hasGraphQL {
			set++
		}
		if set > 1 {
			return fmt.Errorf("service %q: may set only one of ProtoSource (or ProtoFS), OpenAPISpec, GraphQLEndpoint", s.Namespace)
		}
		if set == 0 {
			return fmt.Errorf("service %q: must set ProtoSource (or ProtoFS), OpenAPISpec, or GraphQLEndpoint", s.Namespace)
		}
		depends, owner := s.manifestProto()
		subscribes := s.subscribesProto()
		var verify *cpv1.Verify
		if s.HealthURL != "" || s.SpecURL != "" {
			verify = &cpv1.Verify{HealthUrl: s.HealthURL, SpecUrl: s.SpecURL}
		}
		if hasGraphQL {
			req.Services = append(req.Services, &cpv1.ServiceBinding{
				Namespace:                 s.Namespace,
				Version:                   s.Version,
				GraphqlEndpoint:           s.GraphQLEndpoint,
				MaxConcurrency:            s.MaxConcurrency,
				MaxConcurrencyPerInstance: s.MaxConcurrencyPerInstance,
				Depends:                   depends,
				Owner:                     owner,
				Public:                    s.Public,
				Subscribes:                subscribes,
				Verify:                    verify,
			})
			continue
		}
		if hasOpenAPI {
			req.Services = append(req.Services, &cpv1.ServiceBinding{
				Namespace:                 s.Namespace,
				Version:                   s.Version,
				OpenapiSpec:               s.OpenAPISpec,
				MaxConcurrency:            s.MaxConcurrency,
				MaxConcurrencyPerInstance: s.MaxConcurrencyPerInstance,
				Depends:                   depends,
				Owner:                     owner,
				Public:                    s.Public,
				Subscribes:                subscribes,
				Verify:                    verify,
			})
			continue
		}
		req.Services = append(req.Services, &cpv1.ServiceBinding{
			Namespace:                 s.Namespace,
			Version:                   s.Version,
			ProtoSource:               protoSource,
			ProtoImports:              protoImports,
			MaxConcurrency:            s.MaxConcurrency,
			MaxConcurrencyPerInstance: s.MaxConcurrencyPerInstance,
			Depends:                   depends,
			Owner:                     owner,
			Public:                    s.Public,
			Subscribes:                subscribes,
			Verify:                    verify,
		})
	}

	resp, err := r.gateway().client.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("Register: %w", err)
	}
	r.mu.Lock()
	r.id = resp.GetRegistrationId()
	r.ttl = time.Duration(resp.GetTtlSeconds()) * time.Second
	r.bindings = bindingsFromProto(resp.GetBindings())
	r.mu.Unlock()
	return nil
}

func (r *Registration) heartbeatLoop(interval time.Duration) {
	defer close(r.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	timeout := interval
	if timeout > heartbeatTimeoutMax {
		timeout = heartbeatTimeoutMax
	}
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.beat(timeout)
		}
	}
}

// heartbeatTimeoutMax bounds one Heartbeat call, so a gateway that
// hangs (partition, frozen process) counts as unreachable.
const heartbeatTimeoutMax = 5 * time.Second

// registerTimeout bounds one Register call made from the heartbeat
// loop. Register is slower than Heartbeat: the gateway compiles the
// proto, or introspects a GraphQL endpoint.
const registerTimeout = 10 * time.Second

// beat sends one heartbeat. Not ok (the gateway does not know the
// registration) → register again on the same gateway. Unreachable →
// move to the next gateway address, if there is one.
func (r *Registration) beat(timeout time.Duration) {
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	if id == "" {
		return
	}
	gw := r.gateway()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	resp, err := gw.client.Heartbeat(ctx, &cpv1.HeartbeatRequest{RegistrationId: id})
	cancel()
	if err != nil {
		r.logger("controlclient: heartbeat to %s: %v", gw.addr, err)
		r.failover(gw)
		return
	}
	if !resp.GetOk() {
		r.logger("controlclient: gateway %s does not know registration %s, re-registering", gw.addr, id)
		ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
		err := r.register(ctx)
		cancel()
		if err != nil {
			r.logger("controlclient: re-register on %s failed: %v", gw.addr, err)
			r.failover(gw)
		}
		return
	}
	r.setBindings(resp.GetBindings())
}

// failover registers on the gateway addresses after failed, in order,
// until one accepts. No-op with a single address: the next heartbeat
// tries the same gateway again. When every other address fails too,
// the next heartbeat goes to the last one tried.
func (r *Registration) failover(failed *gatewayConn) {
	n := len(r.gws)
	if n < 2 {
		return
	}
	start := r.indexOf(failed)
	for i := 1; i < n; i++ {
		idx := (start + i) % n
		r.mu.Lock()
		r.cur = idx
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
		err := r.register(ctx)
		cancel()
		if err == nil {
			r.logger("controlclient: registered on gateway %s instead of %s (registration %s)", r.gws[idx].addr, failed.addr, r.ID())
			return
		}
		r.logger("controlclient: register on %s failed: %v", r.gws[idx].addr, err)
	}
}

func (r *Registration) indexOf(gw *gatewayConn) int {
	for i, g := range r.gws {
		if g == gw {
			return i
		}
	}
	return 0
}

// gatewayConn is one control-plane address and its client.
type gatewayConn struct {
	addr   string
	conn   *grpc.ClientConn
	client cpv1.ControlPlaneClient
}

// gateway returns the gateway that holds the registration.
func (r *Registration) gateway() *gatewayConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gws[r.cur]
}

// GatewayAddr returns the control-plane address that holds the
// registration now: one of Options.GatewayAddr / GatewayAddrs.
//
// Stability: stable
func (r *Registration) GatewayAddr() string {
	return r.gateway().addr
}

// ID returns the registration ID. It changes when the registration is
// made again (gateway restart, failover to another address).
//
// Stability: stable
func (r *Registration) ID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id
}

// gatewayAddrs is GatewayAddr then GatewayAddrs, without empty and
// repeated entries.
func gatewayAddrs(opts Options) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range append([]string{opts.GatewayAddr}, opts.GatewayAddrs...) {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func (r *Registration) closeConns() error {
	var first error
	for _, g := range r.gws {
		if err := g.conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Close stops the heartbeat and gracefully deregisters. Safe to call
// more than once (and concurrently with the DrainOnSIGTERM path);
// later calls return the first call's result. ctx is used only for
// the Deregister call.
//
// Stability: stable
func (r *Registration) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		if r.sigStop != nil {
			close(r.sigStop)
		}
		close(r.stop)
		<-r.done

		r.mu.Lock()
		id := r.id
		r.mu.Unlock()
		if id != "" {
			_, _ = r.gateway().client.Deregister(ctx, &cpv1.DeregisterRequest{RegistrationId: id})
		}
		r.closeErr = r.closeConns()
	})
	return r.closeErr
}

// resolveProtoFS walks fsys, returns the bytes of `entry` as the
// proto_source plus a map of every other .proto under the FS as
// proto_imports (keyed by their path within fsys). Used by
// Service.ProtoFS to flatten a multi-file .proto layout into the
// (source, imports-map) shape the wire carries.
//
// Filenames in the imports map use the same path the proto's
// `import "..."` statements use — protocompile resolves both
// against the same string. This means imports must be referenced
// the same way they're keyed in fsys (typically just the basename,
// e.g. "auth.proto", since `import "auth.proto";` is the convention
// for co-located files). Caller responsibility to align fsys layout
// with the import paths in the entrypoint .proto.
func resolveProtoFS(fsys fs.FS, entry string) ([]byte, map[string][]byte, error) {
	src, err := fs.ReadFile(fsys, entry)
	if err != nil {
		return nil, nil, fmt.Errorf("read entry %q: %w", entry, err)
	}
	imports := map[string][]byte{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".proto") {
			return nil
		}
		// Strip leading "./" if present and normalise; entry may be
		// either basename ("greeter.proto") or path ("foo/x.proto") —
		// match the WalkDir-emitted shape.
		key := path.Clean(p)
		if key == path.Clean(entry) {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		imports[key] = body
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(imports) == 0 {
		imports = nil
	}
	return src, imports, nil
}
