package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// registryValue is the JSON payload stored in the registry KV bucket
// under each replica key. The descriptor bytes are the raw
// FileDescriptorSet that was sent on Register — every gateway can
// reconstruct the FileDescriptor and dial the replica from this alone.
type registryValue struct {
	RegID             string `json:"reg_id"`
	Namespace         string `json:"namespace"`
	Version           string `json:"version"`
	ReplicaID         string `json:"replica_id"`
	Addr              string `json:"addr"`
	InstanceID        string `json:"instance_id,omitempty"`
	FileName          string `json:"file_name,omitempty"`
	FileDescriptorSet []byte `json:"fd_set"`
	Hash              []byte `json:"hash"`
	OwnerNodeID       string `json:"owner_node_id"`
}

// replicaKey returns the KV key for a given (namespace, version, replica_id).
// Format: "pool.<ns>.<ver>.<replica_id>". All three components must be
// dot-free so the NATS subject tokenisation lines up with watch globs.
func replicaKey(ns, ver, replicaID string) string {
	return fmt.Sprintf("pool.%s.%s.%s", ns, ver, replicaID)
}

func parseReplicaKey(key string) (ns, ver, replicaID string, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 4 || parts[0] != "pool" {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}

// validateNS returns an error if ns contains a '.' or is empty — both
// would break the registry key encoding.
func validateNS(ns string) error {
	if ns == "" {
		return fmt.Errorf("namespace is empty")
	}
	if strings.ContainsAny(ns, ".*>/$") {
		return fmt.Errorf("namespace %q contains reserved character", ns)
	}
	return nil
}

func newReplicaID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// putRegistryValue serialises v and writes it under its replica key.
func putRegistryValue(ctx context.Context, kv jetstream.KeyValue, v registryValue) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal registry value: %w", err)
	}
	if _, err := kv.Put(ctx, replicaKey(v.Namespace, v.Version, v.ReplicaID), b); err != nil {
		return fmt.Errorf("kv put: %w", err)
	}
	return nil
}

func deleteRegistryKey(ctx context.Context, kv jetstream.KeyValue, ns, ver, replicaID string) error {
	if err := kv.Delete(ctx, replicaKey(ns, ver, replicaID)); err != nil {
		return fmt.Errorf("kv delete: %w", err)
	}
	return nil
}

// kvCallTimeout is the per-call deadline for KV operations. Kept short
// so a wedged JS meta election doesn't block client RPCs indefinitely.
const kvCallTimeout = 5 * time.Second

func kvCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, kvCallTimeout)
}
