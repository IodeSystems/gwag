package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// registryValue is the JSON payload stored in the registry KV bucket
// under each replica key. Carries one of three possible service
// shapes: a ProtoSource (raw .proto bytes for gRPC), an OpenAPISpec
// (HTTP), or a GraphQLEndpoint + GraphQLIntrospection (downstream
// GraphQL). Reconciler disambiguates via IsOpenAPI / IsGraphQL;
// when neither is set, the entry is treated as proto.
type registryValue struct {
	RegID       string `json:"reg_id"`
	Namespace   string `json:"namespace"`
	Version     string `json:"version"`
	ReplicaID   string `json:"replica_id"`
	Addr        string `json:"addr"`
	InstanceID  string `json:"instance_id,omitempty"`
	OwnerNodeID string `json:"owner_node_id"`
	Hash        []byte `json:"hash"`

	// Proto path: raw .proto entrypoint bytes plus optional transitive
	// imports keyed by import path. Sibling of OpenAPISpec — both
	// shapes ship raw source and the gateway compiles on receive.
	ProtoSource  []byte            `json:"proto_source,omitempty"`
	ProtoImports map[string][]byte `json:"proto_imports,omitempty"`

	// OpenAPI path: raw spec bytes (JSON or YAML).
	OpenAPISpec []byte `json:"openapi_spec,omitempty"`

	// GraphQL path: the endpoint URL the source forwards dispatches
	// to, plus the introspection-result JSON the receiving gateway
	// fetched at Register time. Caching the introspection means
	// other peers' reconcilers don't have to re-fetch.
	GraphQLEndpoint      string `json:"graphql_endpoint,omitempty"`
	GraphQLIntrospection []byte `json:"graphql_introspection,omitempty"`

	// Per-binding concurrency caps from the ServiceBinding. Frozen at
	// first registration so the reconciler can size pool / source
	// sems consistently across the cluster. 0 → gateway default
	// (service-level) or unbounded (per-instance).
	MaxConcurrency            uint32 `json:"max_concurrency,omitempty"`
	MaxConcurrencyPerInstance uint32 `json:"max_concurrency_per_instance,omitempty"`
}

// IsOpenAPI reports whether this entry represents an OpenAPI source.
func (v *registryValue) IsOpenAPI() bool { return len(v.OpenAPISpec) > 0 }

// IsGraphQL reports whether this entry represents a downstream
// GraphQL source.
func (v *registryValue) IsGraphQL() bool { return v.GraphQLEndpoint != "" }

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

// kvCallTimeout is the per-call deadline for KV operations. Kept short
// so a wedged JS meta election doesn't block client RPCs indefinitely.
const kvCallTimeout = 5 * time.Second

func kvCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, kvCallTimeout)
}

// kvPutRetry puts value under key, retrying until ctx ends. A put
// sent while a bucket's replica count changes (reconcileReplicas
// raises it as peers join) can get no ack: the stream elects a new
// leader and the publish waits out its whole deadline. Each attempt
// gets kvPutAttemptTimeout, so a lost put costs that long, not all of
// ctx. Only for writes where repeating the same put is safe.
func kvPutRetry(ctx context.Context, kv jetstream.KeyValue, key string, value []byte) error {
	for {
		actx, cancel := context.WithTimeout(ctx, kvPutAttemptTimeout)
		_, err := kv.Put(actx, key, value)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// kvPutAttemptTimeout bounds one put in kvPutRetry. A healthy put
// acks in milliseconds.
const kvPutAttemptTimeout = 2 * time.Second
