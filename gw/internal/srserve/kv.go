// Package srserve serves the Confluent Schema Registry REST API from a
// small key-value store: in memory for a single gateway, a JetStream
// KV bucket for a cluster. The gateway owns the wiring
// (gw/schema_registry.go); this package knows nothing of slots.
package srserve

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
)

// ErrNotFound: no value at the key.
var ErrNotFound = errors.New("srserve: key not found")

// ErrConflict: Create of a key that exists, or Update at a revision
// that is no longer the latest.
var ErrConflict = errors.New("srserve: key exists or changed")

// KV is the storage the registry needs. Keys use the JetStream KV
// alphabet ([-/_=.a-zA-Z0-9]).
type KV interface {
	Get(ctx context.Context, key string) ([]byte, uint64, error)
	Create(ctx context.Context, key string, val []byte) (uint64, error)
	Update(ctx context.Context, key string, val []byte, rev uint64) (uint64, error)
	Put(ctx context.Context, key string, val []byte) (uint64, error)
	// Delete removes key. rev > 0: only while rev is its latest
	// revision (else ErrConflict). A missing key is not an error.
	Delete(ctx context.Context, key string, rev uint64) error
	// Keys lists the keys that start with prefix (which ends in ".").
	Keys(ctx context.Context, prefix string) ([]string, error)
}

// MemKV is a KV in memory.
type MemKV struct {
	mu  sync.Mutex
	rev uint64
	m   map[string]memEntry
}

type memEntry struct {
	val []byte
	rev uint64
}

// NewMemKV returns an empty MemKV.
func NewMemKV() *MemKV { return &MemKV{m: map[string]memEntry{}} }

func (k *MemKV) Get(_ context.Context, key string) ([]byte, uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.m[key]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return append([]byte(nil), e.val...), e.rev, nil
}

func (k *MemKV) Create(_ context.Context, key string, val []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.m[key]; ok {
		return 0, ErrConflict
	}
	return k.setLocked(key, val), nil
}

func (k *MemKV) Update(_ context.Context, key string, val []byte, rev uint64) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.m[key]; !ok || e.rev != rev {
		return 0, ErrConflict
	}
	return k.setLocked(key, val), nil
}

func (k *MemKV) Put(_ context.Context, key string, val []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.setLocked(key, val), nil
}

func (k *MemKV) Delete(_ context.Context, key string, rev uint64) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.m[key]
	if rev > 0 && (!ok || e.rev != rev) {
		return ErrConflict
	}
	delete(k.m, key)
	return nil
}

func (k *MemKV) setLocked(key string, val []byte) uint64 {
	k.rev++
	k.m[key] = memEntry{val: append([]byte(nil), val...), rev: k.rev}
	return k.rev
}

func (k *MemKV) Keys(_ context.Context, prefix string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for key := range k.m {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// JetStreamKV adapts a JetStream KV bucket.
type JetStreamKV struct{ KV jetstream.KeyValue }

func jsErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyDeleted):
		return ErrNotFound
	case errors.Is(err, jetstream.ErrKeyExists), errors.Is(err, jetstream.ErrKeyRevisionMismatch):
		return ErrConflict
	}
	return err
}

func (k JetStreamKV) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	e, err := k.KV.Get(ctx, key)
	if err != nil {
		return nil, 0, jsErr(err)
	}
	return e.Value(), e.Revision(), nil
}

func (k JetStreamKV) Create(ctx context.Context, key string, val []byte) (uint64, error) {
	rev, err := k.KV.Create(ctx, key, val)
	return rev, jsErr(err)
}

func (k JetStreamKV) Update(ctx context.Context, key string, val []byte, rev uint64) (uint64, error) {
	r, err := k.KV.Update(ctx, key, val, rev)
	return r, jsErr(err)
}

func (k JetStreamKV) Put(ctx context.Context, key string, val []byte) (uint64, error) {
	rev, err := k.KV.Put(ctx, key, val)
	return rev, jsErr(err)
}

func (k JetStreamKV) Delete(ctx context.Context, key string, rev uint64) error {
	var opts []jetstream.KVDeleteOpt
	if rev > 0 {
		opts = append(opts, jetstream.LastRevision(rev))
	}
	err := jsErr(k.KV.Delete(ctx, key, opts...))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (k JetStreamKV) Keys(ctx context.Context, prefix string) ([]string, error) {
	l, err := k.KV.ListKeysFiltered(ctx, prefix+">")
	if err != nil {
		return nil, jsErr(err)
	}
	defer l.Stop()
	var out []string
	for key := range l.Keys() {
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}
