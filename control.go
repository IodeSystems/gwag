package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	defer g.mu.Unlock()
	if g.cp == nil {
		g.cp = &controlPlane{
			gw:    g,
			regs:  map[string]*registration{},
			conns: map[string]*sharedConn{},
		}
		go g.cp.janitor()
	}
	return g.cp
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
		fileDesc  protoreflect.FileDescriptor
	}
	prep := make([]prepared, 0, len(req.GetServices()))
	usedNS := map[string]bool{}
	for _, b := range req.GetServices() {
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
		if usedNS[ns] {
			return nil, fmt.Errorf("controlplane: duplicate namespace in request: %q", ns)
		}
		usedNS[ns] = true
		prep = append(prep, prepared{namespace: ns, fileDesc: fd})
	}

	id := newRegID()

	// Single critical section: acquire shared conn, add services,
	// reassemble. On failure, roll back partial state.
	cp.mu.Lock()
	defer cp.mu.Unlock()

	cp.gw.mu.Lock()
	defer cp.gw.mu.Unlock()

	sc, err := cp.acquireConnLocked(req.GetAddr())
	if err != nil {
		return nil, fmt.Errorf("controlplane: dial %s: %w", req.GetAddr(), err)
	}

	added := 0
	for _, p := range prep {
		err := cp.gw.addServiceLocked(&registeredService{
			namespace: p.namespace,
			file:      p.fileDesc,
			conn:      sc.conn,
			owner:     id,
		})
		if err != nil {
			// Rollback: remove what we just added, release conn.
			_, _ = cp.gw.removeServicesByOwnerLocked(id)
			cp.releaseConnLocked(req.GetAddr())
			return nil, err
		}
		added++
	}
	_ = added

	reg := &registration{
		id:       id,
		addr:     req.GetAddr(),
		instance: req.GetInstanceId(),
		ttl:      ttl,
		conn:     sc,
	}
	for _, p := range prep {
		reg.namespaces = append(reg.namespaces, p.namespace)
	}
	reg.lastBeatMs.Store(time.Now().UnixMilli())
	cp.regs[id] = reg

	return &cpv1.RegisterResponse{
		RegistrationId: id,
		TtlSeconds:     uint32(ttl / time.Second),
	}, nil
}

func (cp *controlPlane) Heartbeat(ctx context.Context, req *cpv1.HeartbeatRequest) (*cpv1.HeartbeatResponse, error) {
	cp.mu.Lock()
	reg, ok := cp.regs[req.GetRegistrationId()]
	cp.mu.Unlock()
	if !ok {
		return &cpv1.HeartbeatResponse{Ok: false}, nil
	}
	reg.lastBeatMs.Store(time.Now().UnixMilli())
	return &cpv1.HeartbeatResponse{Ok: true}, nil
}

func (cp *controlPlane) Deregister(ctx context.Context, req *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	reg, ok := cp.regs[req.GetRegistrationId()]
	if !ok {
		return &cpv1.DeregisterResponse{}, nil
	}
	delete(cp.regs, reg.id)

	cp.gw.mu.Lock()
	_, _ = cp.gw.removeServicesByOwnerLocked(reg.id)
	cp.gw.mu.Unlock()

	cp.releaseConnLocked(reg.addr)
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
