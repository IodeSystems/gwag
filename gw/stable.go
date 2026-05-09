package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// stableVN tracks the highest-ever-seen "vN" cut per namespace.
// Plan §4 stable-alias support: at schema render time, if
// stableVN[ns] > 0 and a service with matching version is currently
// in the build, the renderer emits a `stable` sub-field on the
// namespace container aliasing that service's content.
//
// Monotonic — only advances. advanceStableLocked never decreases
// the recorded value, so deregistration of the current stable's vN
// does not roll the alias back. The accompanying RetractStable admin
// RPC (separate todo, plan §4) is the only path that decrements.
//
// Standalone gateways keep this map purely in-process. Cluster-mode
// gateways persist it in the dedicated `go-api-gateway-stable` KV
// bucket (TTL=0): a TTL-less monotonic value per namespace. Each
// node watches the bucket and converges its local map; writeback on
// advance uses CAS so concurrent peers picking different values
// converge on the maximum. The bucket is the only way a fresh node
// joining mid-life recovers the historical max when the target vN's
// last replica is currently absent.

// stableKVValue is the JSON payload written to the stable KV bucket.
// Just an integer wrapped in JSON so future fields (RetractedAt,
// RetractedBy) can be added without a wire-format break.
type stableKVValue struct {
	VN int `json:"vN"`
}

// advanceStableLocked records ns's vN as a candidate for the
// per-namespace "stable" alias. Only advances; never decreases.
// Called from every kind's registration path after the slot has
// been allocated. Caller holds g.mu. vN <= 0 (i.e. version=="unstable")
// is a no-op.
//
// Cluster mode: when the local value advances, kicks off a best-
// effort async writeback to the stable KV bucket. The watch loop on
// every peer reflects the final result back into the local map.
func (g *Gateway) advanceStableLocked(ns string, vN int) {
	if vN <= 0 {
		return
	}
	if g.stableVN == nil {
		g.stableVN = map[string]int{}
	}
	if cur := g.stableVN[ns]; vN > cur {
		g.stableVN[ns] = vN
		if t := g.peers; t != nil && t.stable != nil {
			go writeStableMonotonic(g.life, t.stable, ns, vN)
		}
	}
}

// stableSnapshotLocked returns a snapshot of the stable_vN map for
// passing into RenderGraphQLRuntimeFields. Returns nil when no
// namespace has registered a `vN` cut so far (so the renderer can
// short-circuit). Caller holds g.mu.
func (g *Gateway) stableSnapshotLocked() map[string]int {
	if len(g.stableVN) == 0 {
		return nil
	}
	out := make(map[string]int, len(g.stableVN))
	for k, v := range g.stableVN {
		out[k] = v
	}
	return out
}

// observeStableFromKVLocked applies a value seen in the stable KV
// bucket to the local map, monotonically. Caller holds g.mu.
// Returns true when the local map advanced.
func (g *Gateway) observeStableFromKVLocked(ns string, vN int) bool {
	if vN <= 0 {
		return false
	}
	if g.stableVN == nil {
		g.stableVN = map[string]int{}
	}
	if cur := g.stableVN[ns]; vN > cur {
		g.stableVN[ns] = vN
		return true
	}
	return false
}

// writeStableMonotonic writes vN under key=ns in the stable bucket
// using a CAS loop so concurrent peers writing different values
// converge on the maximum. Best-effort: any error short of "key is
// already at a higher value" is dropped — the watch loop on every
// peer reflects whatever final value lands back into the local map
// regardless of which writer won.
//
// Bounded retry budget: CAS races in a healthy cluster resolve in
// 1-2 rounds, but the first-ever write can race JetStream stream-
// leader election (multi-second on a fresh cluster). 10 attempts
// with a 250ms backoff gives ~2.5s of write attempts plus the per-
// call timeout headroom — enough to ride out election without
// blocking the goroutine indefinitely on a wedged cluster.
func writeStableMonotonic(parent context.Context, kv jetstream.KeyValue, ns string, vN int) {
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithTimeout(parent, kvCallTimeout)
		err := tryWriteStable(ctx, kv, ns, vN)
		cancel()
		if err == nil || errors.Is(err, errStableNotHigher) {
			return
		}
		select {
		case <-parent.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// errStableNotHigher signals "current KV value already meets or
// exceeds vN" — a normal outcome, not a retry condition.
var errStableNotHigher = errors.New("stable: KV already at >= vN")

func tryWriteStable(ctx context.Context, kv jetstream.KeyValue, ns string, vN int) error {
	entry, err := kv.Get(ctx, ns)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("stable: get %s: %w", ns, err)
	}
	cur := 0
	var rev uint64
	if entry != nil {
		v, perr := parseStableKVValue(entry.Value())
		if perr != nil {
			// Malformed payload — overwrite below.
			cur = 0
		} else {
			cur = v
		}
		rev = entry.Revision()
	}
	if vN <= cur {
		return errStableNotHigher
	}
	payload, _ := json.Marshal(stableKVValue{VN: vN})
	if rev == 0 {
		// No prior key — Create rejects on race so concurrent peers
		// don't both create with different values.
		_, err = kv.Create(ctx, ns, payload)
	} else {
		_, err = kv.Update(ctx, ns, payload, rev)
	}
	if err != nil {
		return fmt.Errorf("stable: write %s: %w", ns, err)
	}
	return nil
}

// parseStableKVValue extracts the integer from the JSON payload,
// tolerating older bare-integer payloads if they ever existed.
func parseStableKVValue(raw []byte) (int, error) {
	if len(raw) == 0 {
		return 0, errors.New("empty payload")
	}
	var v stableKVValue
	if err := json.Unmarshal(raw, &v); err == nil && v.VN > 0 {
		return v.VN, nil
	}
	// Fallback: bare decimal integer.
	if n, err := strconv.Atoi(string(raw)); err == nil && n > 0 {
		return n, nil
	}
	return 0, fmt.Errorf("stable: unparseable payload %q", string(raw))
}

// stableWatchLoop subscribes to the stable bucket and pushes every
// observed value into g.stableVN monotonically. The initial-state
// replay (before the first nil-marker update) primes a freshly-
// joining node with the cluster's historical maxima.
func (t *peerTracker) stableWatchLoop(ctx context.Context) {
	defer close(t.stableDone)
	for {
		w, err := t.stable.WatchAll(ctx, jetstream.IncludeHistory())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if t.gw.cfg.cluster != nil && t.gw.cfg.cluster.Server != nil {
				t.gw.cfg.cluster.Server.Warnf("stable watch: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		ready := false
		for ev := range w.Updates() {
			if ev == nil {
				ready = true
				continue
			}
			ns := ev.Key()
			switch ev.Operation() {
			case jetstream.KeyValuePut:
				vN, perr := parseStableKVValue(ev.Value())
				if perr != nil {
					if t.gw.cfg.cluster != nil && t.gw.cfg.cluster.Server != nil {
						t.gw.cfg.cluster.Server.Warnf("stable watch: bad value at %s: %v", ns, perr)
					}
					continue
				}
				t.gw.mu.Lock()
				advanced := t.gw.observeStableFromKVLocked(ns, vN)
				rebuild := advanced && ready && t.gw.schema.Load() != nil
				t.gw.mu.Unlock()
				if rebuild {
					t.gw.mu.Lock()
					_ = t.gw.assembleLocked()
					t.gw.mu.Unlock()
				}
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				// Plan §4: stable is monotonic. Deletes from the bucket
				// are operator actions (RetractStable, separate todo).
				// Until that's wired, ignore — the local in-memory map
				// stays put.
			}
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}
