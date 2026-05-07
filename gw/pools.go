package gateway

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// marshalFileDescriptorSet serializes fd plus its transitive imports
// into a FileDescriptorSet — what `protoc -o` would emit and what the
// control plane expects in ServiceBinding.file_descriptor_set.
//
// Output is canonical: files are sorted by path and the protobuf
// encoding is deterministic, so two callers that built the same
// descriptor with different protoc versions produce identical bytes
// (and identical hashes) as long as the structural content matches.
func marshalFileDescriptorSet(fd protoreflect.FileDescriptor) ([]byte, error) {
	fds := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var walk func(f protoreflect.FileDescriptor)
	walk = func(f protoreflect.FileDescriptor) {
		if seen[string(f.Path())] {
			return
		}
		seen[string(f.Path())] = true
		for i := 0; i < f.Imports().Len(); i++ {
			walk(f.Imports().Get(i).FileDescriptor)
		}
		fds.File = append(fds.File, protodesc.ToFileDescriptorProto(f))
	}
	walk(fd)
	sort.Slice(fds.File, func(i, j int) bool {
		return fds.File[i].GetName() < fds.File[j].GetName()
	})
	return proto.MarshalOptions{Deterministic: true}.Marshal(fds)
}

// poolKey identifies a pool by namespace + version. Namespace defaults
// to filename stem; version defaults to v1. Pool key is the unit of
// schema participation — distinct keys mean distinct GraphQL fields.
type poolKey struct {
	namespace string
	version   string
}

// pool aggregates one or more replicas serving the same proto under
// the same namespace+version. Membership is gated by file_descriptor
// hash equality — replicas claiming the namespace with a different
// proto are rejected.
type pool struct {
	key      poolKey
	versionN int
	file     protoreflect.FileDescriptor
	hash     [32]byte

	// replicas slice is replaced (not mutated in place) on add/remove
	// so dispatch closures snapshotting it never see partial mutation.
	// Reads via Load() in pickReplica.
	replicas atomic.Pointer[[]*replica]

	// sem caps simultaneous unary dispatches against this pool. nil
	// when MaxInflight is 0 (unbounded). Buffered channel; send to
	// acquire, receive to release.
	sem chan struct{}

	// streamSem caps simultaneous active subscription streams. nil
	// when MaxStreams is 0 (unbounded). Independent of sem so
	// long-lived streams don't crowd out unary queries.
	streamSem chan struct{}

	// queueing / streamQueueing track waiters on each semaphore.
	queueing       atomic.Int32
	streamQueueing atomic.Int32

	// streamInflight tracks active subscription streams against this
	// pool, surfaced as the streams_inflight gauge.
	streamInflight atomic.Int32
}

// replica is one backend behind a pool. inflight is incremented before
// gRPC Invoke and decremented after; pickReplica picks the lowest.
type replica struct {
	id       string // KV-side replica id; "" for boot-time AddProto entries
	addr     string
	owner    string // registration ID
	conn     grpc.ClientConnInterface
	inflight atomic.Int32
}

// pickReplica returns the replica with the lowest in-flight count.
// Returns nil if the pool has no replicas (transient state during
// drain). Caller treats nil as a transient failure.
func (p *pool) pickReplica() *replica {
	rs := p.replicas.Load()
	if rs == nil || len(*rs) == 0 {
		return nil
	}
	best := (*rs)[0]
	bestN := best.inflight.Load()
	for _, r := range (*rs)[1:] {
		n := r.inflight.Load()
		if n < bestN {
			best, bestN = r, n
		}
	}
	return best
}

// addReplica returns a new pool slice with `r` appended. The pool's
// replicas pointer is swapped atomically by the caller.
func (p *pool) addReplica(r *replica) {
	cur := p.replicas.Load()
	var next []*replica
	if cur != nil {
		next = make([]*replica, 0, len(*cur)+1)
		next = append(next, *cur...)
	}
	next = append(next, r)
	p.replicas.Store(&next)
}

// removeReplicasByOwner returns the count removed. Empty pools are
// caller-detected via Load().Len() == 0 after.
func (p *pool) removeReplicasByOwner(owner string) int {
	cur := p.replicas.Load()
	if cur == nil {
		return 0
	}
	next := make([]*replica, 0, len(*cur))
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
	p.replicas.Store(&next)
	return removed
}

// removeReplicaByID drops the replica with the given KV id, returning
// the removed *replica or nil if not present.
func (p *pool) removeReplicaByID(id string) *replica {
	cur := p.replicas.Load()
	if cur == nil || id == "" {
		return nil
	}
	next := make([]*replica, 0, len(*cur))
	var removed *replica
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
	p.replicas.Store(&next)
	return removed
}

// findReplicaByID returns the replica with the given id, or nil.
func (p *pool) findReplicaByID(id string) *replica {
	cur := p.replicas.Load()
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

func (p *pool) replicaCount() int {
	cur := p.replicas.Load()
	if cur == nil {
		return 0
	}
	return len(*cur)
}

// parseVersion turns "v3", "3", "v10" into 3, 3, 10. Empty string is
// treated as v1. Anything not parseable returns an error and the
// registration is rejected — keeps "latest" semantics unambiguous.
func parseVersion(s string) (canonical string, n int, err error) {
	if s == "" {
		return "v1", 1, nil
	}
	digits := s
	if strings.HasPrefix(digits, "v") || strings.HasPrefix(digits, "V") {
		digits = digits[1:]
	}
	n, err = strconv.Atoi(digits)
	if err != nil || n < 0 {
		return "", 0, fmt.Errorf("version %q: must be vN or N (non-negative integer)", s)
	}
	return "v" + strconv.Itoa(n), n, nil
}

// hashDescriptorSet returns SHA256 of a canonicalised FileDescriptorSet.
// b is the raw wire bytes a client sent; we unmarshal and re-marshal
// via marshalFileDescriptorSet so the hash depends on structural
// content, not on the client's protobuf marshalling.
func hashDescriptorSet(b []byte) ([32]byte, error) {
	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(b, fds); err != nil {
		return [32]byte{}, fmt.Errorf("hash: unmarshal: %w", err)
	}
	sort.Slice(fds.File, func(i, j int) bool {
		return fds.File[i].GetName() < fds.File[j].GetName()
	})
	canon, err := proto.MarshalOptions{Deterministic: true}.Marshal(fds)
	if err != nil {
		return [32]byte{}, fmt.Errorf("hash: marshal: %w", err)
	}
	return sha256.Sum256(canon), nil
}

// hashFromFileDescriptor builds a FileDescriptorSet from fd plus its
// transitive imports, canonicalises it, and hashes that. Used by
// AddProto (which loads from disk and never sees wire bytes).
func hashFromFileDescriptor(fd protoreflect.FileDescriptor) ([32]byte, error) {
	b, err := marshalFileDescriptorSet(fd)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}
