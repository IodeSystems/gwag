package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
)

const (
	defaultTTL    = 30 * time.Second
	janitorPeriod = 5 * time.Second
)

// ControlPlane returns a gRPC service implementation that lets remote
// services register themselves with this gateway. Mount on a
// *grpc.Server bound to whatever address you want services to call.
//
//	srv := grpc.NewServer()
//	cpv1.RegisterControlPlaneServer(srv, gw.ControlPlane())
//	go srv.Serve(lis)
//
// The first call to ControlPlane starts the heartbeat janitor; safe to
// call multiple times — the same impl is returned.
func (g *Gateway) ControlPlane() cpv1.ControlPlaneServer {
	g.mu.Lock()
	if g.cp == nil {
		g.cp = &controlPlane{
			gw:    g,
			regs:  map[string]*registration{},
			conns: map[string]*sharedConn{},
		}
		go g.cp.janitor()
	}
	cp := g.cp
	g.mu.Unlock()

	// Boot cluster tracking (peers KV + monotonic R) async on first
	// ControlPlane access. It retries until JetStream meta is ready.
	if g.cfg.cluster != nil {
		go g.bootClusterTracking()
	}
	return cp
}

// bootClusterTracking retries startClusterTracking until success or
// gateway shutdown. Necessary because JetStream meta leader election
// races boot — the buckets can't be created until meta is up.
func (g *Gateway) bootClusterTracking() {
	backoff := 250 * time.Millisecond
	const maxBackoff = 5 * time.Second
	for {
		if g.life.Err() != nil {
			return
		}
		g.mu.Lock()
		started := g.peers != nil
		g.mu.Unlock()
		if started {
			return
		}
		if _, err := g.startClusterTracking(g.life); err == nil {
			return
		} else {
			g.cfg.cluster.Server.Warnf("gateway: cluster tracking pending: %v", err)
		}
		select {
		case <-g.life.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

type controlPlane struct {
	cpv1.UnimplementedControlPlaneServer

	gw   *Gateway
	mu   sync.Mutex
	regs map[string]*registration

	// conns is the shared-grpc-conn pool keyed by addr. Multiple
	// registrations against the same addr (one binary hosting many
	// services) reuse the same connection; closed on last unref.
	conns map[string]*sharedConn
}

type registration struct {
	id         string
	addr       string
	instance   string
	ttl        time.Duration
	lastBeatMs atomic.Int64
	namespaces []string
	conn       *sharedConn

	// kvKeys is the set of registry KV keys this registration owns.
	// Heartbeat re-Puts each one to refresh the bucket TTL; Deregister
	// deletes them. Empty in non-cluster mode.
	kvKeys []registryKeyRef
}

type registryKeyRef struct {
	namespace string
	version   string
	replicaID string
	value     []byte // pre-marshalled JSON for cheap re-Put on heartbeat
}

type sharedConn struct {
	conn *grpc.ClientConn
	refs int
}

func (cp *controlPlane) Register(ctx context.Context, req *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	if req.GetAddr() == "" {
		return nil, fmt.Errorf("controlplane: addr is required")
	}
	if len(req.GetServices()) == 0 {
		return nil, fmt.Errorf("controlplane: at least one ServiceBinding is required")
	}

	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	if ttl == 0 {
		ttl = defaultTTL
	}

	type prepared struct {
		namespace string
		version   string
		hash      [32]byte

		// Proto path
		fileDesc protoreflect.FileDescriptor
		fdBytes  []byte
		fileName string

		// OpenAPI path
		openAPISpec []byte

		// GraphQL path
		graphqlEndpoint      string
		graphqlIntrospection []byte

		isOpenAPI bool
		isGraphQL bool
	}
	prep := make([]prepared, 0, len(req.GetServices()))
	type nsverKey struct{ ns, ver string }
	used := map[nsverKey]bool{}
	for _, b := range req.GetServices() {
		hasProto := len(b.GetFileDescriptorSet()) > 0
		hasOpenAPI := len(b.GetOpenapiSpec()) > 0
		hasGraphQL := b.GetGraphqlEndpoint() != ""
		setForms := 0
		if hasProto {
			setForms++
		}
		if hasOpenAPI {
			setForms++
		}
		if hasGraphQL {
			setForms++
		}
		if setForms > 1 {
			return nil, fmt.Errorf("controlplane: ServiceBinding may set only one of file_descriptor_set, openapi_spec, graphql_endpoint")
		}
		if setForms == 0 {
			return nil, fmt.Errorf("controlplane: ServiceBinding must set file_descriptor_set, openapi_spec, OR graphql_endpoint")
		}

		if hasGraphQL {
			ns := b.GetNamespace()
			if ns == "" {
				return nil, fmt.Errorf("controlplane: graphql_endpoint binding requires explicit namespace")
			}
			if err := validateNS(ns); err != nil {
				return nil, fmt.Errorf("controlplane: %w", err)
			}
			ver, _, err := parseVersion(b.GetVersion())
			if err != nil {
				return nil, fmt.Errorf("controlplane: %w", err)
			}
			k := nsverKey{ns, ver}
			if used[k] {
				return nil, fmt.Errorf("controlplane: duplicate (namespace=%s, version=%s) in request", ns, ver)
			}
			used[k] = true
			endpoint := b.GetGraphqlEndpoint()
			httpClient := cp.gw.cfg.openAPIHTTP
			if httpClient == nil {
				httpClient = http.DefaultClient
			}
			rawIntro, err := fetchIntrospectionBytes(ctx, httpClient, endpoint)
			if err != nil {
				return nil, fmt.Errorf("controlplane: introspect %s: %w", endpoint, err)
			}
			prep = append(prep, prepared{
				namespace:            ns,
				version:              ver,
				hash:                 hashIntrospection(rawIntro),
				graphqlEndpoint:      endpoint,
				graphqlIntrospection: rawIntro,
				isGraphQL:            true,
			})
			continue
		}

		if hasOpenAPI {
			ns, hash, err := prepOpenAPIBinding(b)
			if err != nil {
				return nil, fmt.Errorf("controlplane: %w", err)
			}
			ver, _, err := parseVersion(b.GetVersion())
			if err != nil {
				return nil, fmt.Errorf("controlplane: %w", err)
			}
			k := nsverKey{ns, ver}
			if used[k] {
				return nil, fmt.Errorf("controlplane: duplicate (namespace=%s, version=%s) in request", ns, ver)
			}
			used[k] = true
			prep = append(prep, prepared{
				namespace:   ns,
				version:     ver,
				hash:        hash,
				openAPISpec: b.GetOpenapiSpec(),
				isOpenAPI:   true,
			})
			continue
		}

		fd, err := parseFileDescriptorSet(b.GetFileDescriptorSet(), b.GetFileName())
		if err != nil {
			return nil, fmt.Errorf("controlplane: descriptor: %w", err)
		}
		ns := b.GetNamespace()
		if ns == "" {
			base := string(fd.Path())
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[i+1:]
			}
			ns = strings.TrimSuffix(base, ".proto")
		}
		if err := validateNS(ns); err != nil {
			return nil, fmt.Errorf("controlplane: %w", err)
		}
		ver, _, err := parseVersion(b.GetVersion())
		if err != nil {
			return nil, fmt.Errorf("controlplane: %w", err)
		}
		k := nsverKey{ns, ver}
		if used[k] {
			return nil, fmt.Errorf("controlplane: duplicate (namespace=%s, version=%s) in request", ns, ver)
		}
		used[k] = true
		hash, err := hashDescriptorSet(b.GetFileDescriptorSet())
		if err != nil {
			return nil, fmt.Errorf("controlplane: %w", err)
		}
		warnUnsupportedStreaming(cp.gw, ns, ver, fd)
		prep = append(prep, prepared{
			namespace: ns,
			version:   ver,
			hash:      hash,
			fileDesc:  fd,
			fdBytes:   b.GetFileDescriptorSet(),
			fileName:  b.GetFileName(),
		})
	}

	id := newRegID()

	// Lookup the cluster KV BEFORE taking g.mu — registryKV takes g.mu
	// internally, and we don't want a re-entrant lock deadlock.
	clusterKV := cp.gw.registryKV()
	ownerNode := cp.gw.nodeID()

	cp.mu.Lock()
	defer cp.mu.Unlock()

	// Cluster mode: KV is the source of truth. Don't dial or touch
	// pools — the reconciler picks up our KV.Put and joins both the
	// receiving gateway's pool and every other gateway's pool.
	if clusterKV != nil {
		var kvKeys []registryKeyRef
		for _, p := range prep {
			replicaID := newReplicaID()
			val := registryValue{
				RegID:       id,
				Namespace:   p.namespace,
				Version:     p.version,
				ReplicaID:   replicaID,
				Addr:        req.GetAddr(),
				InstanceID:  req.GetInstanceId(),
				Hash:        p.hash[:],
				OwnerNodeID: ownerNode,
			}
			switch {
			case p.isGraphQL:
				val.GraphQLEndpoint = p.graphqlEndpoint
				val.GraphQLIntrospection = p.graphqlIntrospection
			case p.isOpenAPI:
				val.OpenAPISpec = p.openAPISpec
			default:
				val.FileName = p.fileName
				val.FileDescriptorSet = p.fdBytes
			}
			b, mErr := json.Marshal(val)
			if mErr != nil {
				cp.deleteKVKeys(ctx, clusterKV, kvKeys)
				return nil, fmt.Errorf("controlplane: marshal kv value: %w", mErr)
			}
			kctx, kcancel := kvCtx(ctx)
			_, pErr := clusterKV.Put(kctx, replicaKey(p.namespace, p.version, replicaID), b)
			kcancel()
			if pErr != nil {
				cp.deleteKVKeys(ctx, clusterKV, kvKeys)
				return nil, fmt.Errorf("controlplane: kv put: %w", pErr)
			}
			kvKeys = append(kvKeys, registryKeyRef{
				namespace: p.namespace,
				version:   p.version,
				replicaID: replicaID,
				value:     b,
			})
		}
		reg := &registration{
			id:       id,
			addr:     req.GetAddr(),
			instance: req.GetInstanceId(),
			ttl:      ttl,
			kvKeys:   kvKeys,
		}
		for _, p := range prep {
			reg.namespaces = append(reg.namespaces, p.namespace+"/"+p.version)
		}
		reg.lastBeatMs.Store(time.Now().UnixMilli())
		cp.regs[id] = reg
		return &cpv1.RegisterResponse{
			RegistrationId: id,
			TtlSeconds:     uint32(ttl / time.Second),
		}, nil
	}

	// Standalone mode: directly populate the in-memory pool and conn
	// pool. OpenAPI bindings don't need a gRPC dial — the addr is an
	// HTTP base URL — so the conn pool is only entered when at least
	// one binding is proto-shaped.
	cp.gw.mu.Lock()
	defer cp.gw.mu.Unlock()

	hasProtoBinding := false
	for _, p := range prep {
		if !p.isOpenAPI && !p.isGraphQL {
			hasProtoBinding = true
			break
		}
	}

	var sc *sharedConn
	if hasProtoBinding {
		var err error
		sc, err = cp.acquireConnLocked(req.GetAddr())
		if err != nil {
			return nil, fmt.Errorf("controlplane: dial %s: %w", req.GetAddr(), err)
		}
	}

	rollback := func() {
		_, _ = cp.gw.removeReplicasByOwnerLocked(id)
		cp.gw.removeOpenAPISourcesByOwnerLocked(id)
		cp.gw.removeGraphQLSourcesByOwnerLocked(id)
		if sc != nil {
			cp.releaseConnLocked(req.GetAddr())
		}
	}
	for _, p := range prep {
		if p.isGraphQL {
			// Standalone path: replicaID unused (no KV-driven removal).
			if err := cp.gw.addGraphQLSourceLocked(p.namespace, p.version, p.graphqlEndpoint, p.graphqlIntrospection, p.hash, id, ""); err != nil {
				rollback()
				return nil, err
			}
			continue
		}
		if p.isOpenAPI {
			// Standalone path: replicaID is unused (no KV-driven
			// removal). Pass "" so addOpenAPISourceLocked treats this
			// as a boot-time-style replica owned by the registration.
			if err := cp.gw.addOpenAPISourceLocked(p.namespace, p.version, req.GetAddr(), p.openAPISpec, p.hash, id, ""); err != nil {
				rollback()
				return nil, err
			}
			continue
		}
		err := cp.gw.joinPoolLocked(poolEntry{
			namespace: p.namespace,
			version:   p.version,
			hash:      p.hash,
			file:      p.fileDesc,
			addr:      req.GetAddr(),
			conn:      sc.conn,
			owner:     id,
		})
		if err != nil {
			rollback()
			return nil, err
		}
	}

	reg := &registration{
		id:       id,
		addr:     req.GetAddr(),
		instance: req.GetInstanceId(),
		ttl:      ttl,
		conn:     sc,
	}
	for _, p := range prep {
		reg.namespaces = append(reg.namespaces, p.namespace+"/"+p.version)
	}
	reg.lastBeatMs.Store(time.Now().UnixMilli())
	cp.regs[id] = reg

	return &cpv1.RegisterResponse{
		RegistrationId: id,
		TtlSeconds:     uint32(ttl / time.Second),
	}, nil
}

// deleteKVKeys best-effort removes all of the given keys from the
// registry KV. Caller passes the bucket explicitly so this is safe to
// call while g.mu is held (registryKV takes g.mu).
func (cp *controlPlane) deleteKVKeys(ctx context.Context, kv jetstream.KeyValue, refs []registryKeyRef) {
	if kv == nil || len(refs) == 0 {
		return
	}
	for _, r := range refs {
		kctx, cancel := kvCtx(ctx)
		_ = kv.Delete(kctx, replicaKey(r.namespace, r.version, r.replicaID))
		cancel()
	}
}

func (cp *controlPlane) Heartbeat(ctx context.Context, req *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	cp.mu.Lock()
	reg, ok := cp.regs[req.GetRegistrationId()]
	cp.mu.Unlock()
	if !ok {
		return &cpv1.HeartbeatResponse{Ok: false}, nil
	}
	reg.lastBeatMs.Store(time.Now().UnixMilli())

	// Cluster mode: refresh each KV entry's TTL by re-Putting the same
	// JSON value. Best-effort; transient KV errors are logged-and-go
	// because the in-memory regs map still has the registration and
	// the next heartbeat will retry.
	if kv := cp.gw.registryKV(); kv != nil {
		for _, r := range reg.kvKeys {
			kctx, cancel := kvCtx(ctx)
			_, _ = kv.Put(kctx, replicaKey(r.namespace, r.version, r.replicaID), r.value)
			cancel()
		}
	}

	return &cpv1.HeartbeatResponse{Ok: true}, nil
}

func (cp *controlPlane) Deregister(ctx context.Context, req *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	cp.mu.Lock()
	reg, ok := cp.regs[req.GetRegistrationId()]
	if !ok {
		cp.mu.Unlock()
		return &cpv1.DeregisterResponse{}, nil
	}
	delete(cp.regs, reg.id)

	// Standalone mode: drop pool replicas + OpenAPI sources owned by
	// this registration, and release the conn if one was acquired
	// (proto-only registrations have a sharedConn; OpenAPI-only
	// registrations don't).
	cp.gw.mu.Lock()
	_, _ = cp.gw.removeReplicasByOwnerLocked(reg.id)
	cp.gw.removeOpenAPISourcesByOwnerLocked(reg.id)
	cp.gw.removeGraphQLSourcesByOwnerLocked(reg.id)
	cp.gw.mu.Unlock()
	if reg.conn != nil {
		cp.releaseConnLocked(reg.addr)
	}
	kvRefs := reg.kvKeys
	cp.mu.Unlock()

	// Cluster mode: KV.Delete fires watch events that drive every
	// gateway's reconciler to drop the replica + release its conn.
	cp.deleteKVKeys(ctx, cp.gw.registryKV(), kvRefs)
	return &cpv1.DeregisterResponse{}, nil
}

func (cp *controlPlane) ListRegistrations(ctx context.Context, _ *cpv1.ListRegistrationsRequest) (*cpv1.ListRegistrationsResponse, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	out := &cpv1.ListRegistrationsResponse{}
	for _, r := range cp.regs {
		out.Registrations = append(out.Registrations, &cpv1.Registration{
			RegistrationId:      r.id,
			Addr:                r.addr,
			InstanceId:          r.instance,
			TtlSeconds:          uint32(r.ttl / time.Second),
			LastHeartbeatUnixMs: uint64(r.lastBeatMs.Load()),
			Namespaces:          append([]string(nil), r.namespaces...),
		})
	}
	return out, nil
}

// warnUnsupportedStreaming logs every method in fd that the gateway
// can't surface — client-streaming and bidi RPCs. Server-streaming is
// promoted to a GraphQL subscription, so we don't warn for those.
// Goes through the embedded NATS logger if cluster is configured,
// fmt.Println otherwise.
func warnUnsupportedStreaming(g *Gateway, ns, ver string, fd protoreflect.FileDescriptor) {
	services := fd.Services()
	for i := 0; i < services.Len(); i++ {
		sd := services.Get(i)
		methods := sd.Methods()
		for j := 0; j < methods.Len(); j++ {
			md := methods.Get(j)
			cs, ss := md.IsStreamingClient(), md.IsStreamingServer()
			var kind string
			switch {
			case cs && ss:
				kind = "bidi"
			case cs && !ss:
				kind = "client-stream"
			default:
				continue
			}
			msg := fmt.Sprintf("gateway: filtering unsupported streaming method %s/%s: /%s/%s (kind=%s)",
				ns, ver, sd.FullName(), md.Name(), kind)
			if g.cfg.cluster != nil {
				g.cfg.cluster.Server.Warnf("%s", msg)
			} else {
				fmt.Println(msg)
			}
		}
	}
}

// ListPeers returns all live peers from the peers KV bucket. Empty
// when running standalone. Order is unspecified.
func (cp *controlPlane) ListPeers(ctx context.Context, _ *cpv1.ListPeersRequest) (*cpv1.ListPeersResponse, error) {
	cp.gw.mu.Lock()
	t := cp.gw.peers
	cp.gw.mu.Unlock()
	if t == nil {
		return &cpv1.ListPeersResponse{}, nil
	}
	out := &cpv1.ListPeersResponse{}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	keys, err := t.peers.Keys(kctx)
	if err != nil {
		// "no keys" is normal when standalone — collapse to empty list.
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return out, nil
		}
		return nil, fmt.Errorf("controlplane: list peers: %w", err)
	}
	for _, k := range keys {
		entry, err := t.peers.Get(kctx, k)
		if err != nil {
			continue
		}
		var pe peerEntry
		if json.Unmarshal(entry.Value(), &pe) == nil {
			out.Peers = append(out.Peers, &cpv1.Peer{
				NodeId:       pe.NodeID,
				Name:         pe.Name,
				JoinedUnixMs: pe.JoinedM,
			})
		}
	}
	return out, nil
}

// ForgetPeer drops a disconnected peer from the peers KV and shrinks
// the registry stream's replica count if appropriate. Refuses if:
//   - the gateway isn't in cluster mode (nothing to forget)
//   - the peer is the local node (forgetting yourself is nonsensical)
//   - the peer is still alive (entry present in peers KV)
func (cp *controlPlane) ForgetPeer(ctx context.Context, req *cpv1.ForgetPeerRequest) (*cpv1.ForgetPeerResponse, error) {
	if req.GetNodeId() == "" {
		return nil, fmt.Errorf("controlplane: node_id is required")
	}
	cp.gw.mu.Lock()
	t := cp.gw.peers
	cp.gw.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("controlplane: cluster not configured")
	}
	if req.GetNodeId() == t.nodeID {
		return nil, fmt.Errorf("controlplane: refuse to forget self (%s)", t.nodeID)
	}

	kctx, cancel := kvCtx(ctx)
	defer cancel()

	// Refuse if the peer is still alive — its KV entry would have been
	// expired by JetStream once the TTL elapsed without a refresh, so
	// presence implies a recent heartbeat.
	if _, err := t.peers.Get(kctx, req.GetNodeId()); err == nil {
		return nil, fmt.Errorf("controlplane: peer %s is still alive — wait for TTL (%s) to expire", req.GetNodeId(), peerTTL)
	} else if !errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, fmt.Errorf("controlplane: peer lookup: %w", err)
	}

	// Bucket auto-expired the entry already. Drop it from our local
	// live set just in case watch hasn't propagated yet, and reconcile.
	t.mu.Lock()
	_, wasLive := t.live[req.GetNodeId()]
	delete(t.live, req.GetNodeId())
	desired := len(t.live)
	t.mu.Unlock()

	if desired > maxReplicas {
		desired = maxReplicas
	}
	if desired < 1 {
		desired = 1
	}
	curR := int(t.currentR.Load())
	resp := &cpv1.ForgetPeerResponse{Removed: wasLive, NewReplicas: uint32(curR)}
	if desired < curR {
		if err := t.setReplicas(ctx, desired); err != nil {
			return nil, fmt.Errorf("controlplane: shrink replicas: %w", err)
		}
		t.currentR.Store(int32(desired))
		resp.NewReplicas = uint32(desired)
	}
	return resp, nil
}

// authorizerNamespace is the reserved registration namespace for the
// SubscriptionAuthorizer delegate. A service implementing the
// AuthorizeSign RPC registers under "_events_auth/v1" and the gateway
// auto-routes SignSubscriptionToken consultations to it.
const authorizerNamespace = "_events_auth"

// SignSubscriptionToken mints an HMAC token for a subscription
// channel, optionally consulting a registered SubscriptionAuthorizer
// delegate before signing. Refer to the proto comment for the policy.
func (cp *controlPlane) SignSubscriptionToken(ctx context.Context, req *cpv1.SignSubscriptionTokenRequest) (*cpv1.SignSubscriptionTokenResponse, error) {
	if req.GetChannel() == "" {
		return nil, fmt.Errorf("controlplane: channel is required")
	}
	if cp.gw.cfg.subAuth.Insecure {
		return &cpv1.SignSubscriptionTokenResponse{
			Code:   cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_NOT_CONFIGURED,
			Reason: "gateway is in insecure-subscribe mode; HMAC signing is disabled",
		}, nil
	}
	if !cp.gw.cfg.subAuth.hasAnySecret() {
		return &cpv1.SignSubscriptionTokenResponse{
			Code:   cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_NOT_CONFIGURED,
			Reason: "subscription secret not configured",
		}, nil
	}
	kid := req.GetKid()
	secret, ok := cp.gw.cfg.subAuth.lookupSecret(kid)
	if !ok {
		// Empty kid + no Secret/Secrets[""] → operator hasn't authorized
		// unkeyed tokens on this gateway. Non-empty kid → rotation set
		// doesn't carry it. Both surface as UNKNOWN_KID so the caller
		// can react identically.
		return &cpv1.SignSubscriptionTokenResponse{
			Code:   cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNKNOWN_KID,
			Reason: fmt.Sprintf("no secret configured for kid %q", kid),
			Kid:    kid,
		}, nil
	}

	timestamp := time.Now().Unix()

	// If a delegate is registered under _events_auth/v1, ask it first.
	// Absence of a delegate means "sign whatever's requested" — callers
	// of SignSubscriptionToken should themselves be authenticated and
	// authorized; the delegate is the additional gate when needed.
	if code, reason, err := cp.gw.consultSubscribeDelegate(ctx, req.GetChannel(), timestamp, req.GetTtlSeconds()); err != nil {
		return &cpv1.SignSubscriptionTokenResponse{
			Code:   cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNAVAILABLE,
			Reason: err.Error(),
		}, nil
	} else if code != cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_UNSPECIFIED && code != cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_OK {
		return &cpv1.SignSubscriptionTokenResponse{Code: code, Reason: reason}, nil
	}

	mac := computeSubscribeHMAC(secret, kid, req.GetChannel(), timestamp)
	return &cpv1.SignSubscriptionTokenResponse{
		Code:          cpv1.SubscribeAuthCode_SUBSCRIBE_AUTH_CODE_OK,
		Hmac:          base64.StdEncoding.EncodeToString(mac),
		TimestampUnix: timestamp,
		Kid:           kid,
	}, nil
}

// ListServices returns one ServiceInfo per (namespace, version) pool
// currently live on this gateway, with the canonical hash and replica
// count. Cross-cluster parity check: dump from two gateways, diff.
func (cp *controlPlane) ListServices(ctx context.Context, _ *cpv1.ListServicesRequest) (*cpv1.ListServicesResponse, error) {
	cp.gw.mu.Lock()
	out := &cpv1.ListServicesResponse{
		Environment: cp.gw.environmentLabel(),
		Services:    make([]*cpv1.ServiceInfo, 0, len(cp.gw.pools)),
	}
	for k, p := range cp.gw.pools {
		if cp.gw.internal[k.namespace] {
			continue
		}
		out.Services = append(out.Services, &cpv1.ServiceInfo{
			Namespace:    k.namespace,
			Version:      k.version,
			HashHex:      hex.EncodeToString(p.hash[:]),
			ReplicaCount: uint32(p.replicaCount()),
		})
	}
	cp.gw.mu.Unlock()
	sort.Slice(out.Services, func(i, j int) bool {
		a, b := out.Services[i], out.Services[j]
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetVersion() < b.GetVersion()
	})
	return out, nil
}

// janitor evicts registrations whose last heartbeat is older than TTL.
// Runs forever; stops only on process exit.
func (cp *controlPlane) janitor() {
	t := time.NewTicker(janitorPeriod)
	defer t.Stop()
	for range t.C {
		now := time.Now().UnixMilli()
		var stale []string
		cp.mu.Lock()
		for id, r := range cp.regs {
			if now-r.lastBeatMs.Load() > r.ttl.Milliseconds() {
				stale = append(stale, id)
			}
		}
		cp.mu.Unlock()
		for _, id := range stale {
			_, _ = cp.Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: id})
		}
	}
}

// ---------------------------------------------------------------------
// Shared connection pool
// ---------------------------------------------------------------------

func (cp *controlPlane) acquireConnLocked(addr string) (*sharedConn, error) {
	if sc, ok := cp.conns[addr]; ok {
		sc.refs++
		return sc, nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	sc := &sharedConn{conn: conn, refs: 1}
	cp.conns[addr] = sc
	return sc, nil
}

func (cp *controlPlane) releaseConnLocked(addr string) {
	sc, ok := cp.conns[addr]
	if !ok {
		return
	}
	sc.refs--
	if sc.refs == 0 {
		_ = sc.conn.Close()
		delete(cp.conns, addr)
	}
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

func newRegID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseFileDescriptorSet decodes the bytes a service shipped over the
// wire and returns the FileDescriptor for the service-bearing file.
// If fileName is empty, the LAST file in the set is used (the
// convention is that FileDescriptorSet emits dependencies first, the
// dependent last).
func parseFileDescriptorSet(b []byte, fileName string) (protoreflect.FileDescriptor, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty file_descriptor_set")
	}
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(b, fds); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(fds.GetFile()) == 0 {
		return nil, fmt.Errorf("file_descriptor_set has no files")
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("protodesc.NewFiles: %w", err)
	}
	target := fileName
	if target == "" {
		target = fds.GetFile()[len(fds.GetFile())-1].GetName()
	}
	fd, err := files.FindFileByPath(target)
	if err != nil {
		return nil, fmt.Errorf("FindFileByPath %s: %w", target, err)
	}
	return fd, nil
}

// Compile-time assertion that protoregistry is wired (helps IDEs).
var _ = (*protoregistry.Files)(nil)
