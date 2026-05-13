package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// MCP surface allowlist storage (plan §Tier 2, MCP integration).
//
// `mcp_config` is a single-key JetStream KV bucket (TTL=0; operator-set
// state must survive restarts). The lone record under
// `mcpConfigKey` is the cluster-wide MCPConfig.
//
// AutoInclude=false (default): the surface is exactly `include` —
// nothing is exposed unless an operator allows it.
// AutoInclude=true: the surface is (all public leaves) − `exclude`.
// Internal `_*` namespaces are filtered first by isInternal; neither
// list can override that.
//
// Each include/exclude entry is a dot-segmented glob (path.Match per
// segment). `*` matches one segment; `**` matches any number of
// segments, including zero.
//
// Standalone gateways keep MCPConfig purely in-process; cluster-mode
// gateways persist it in the bucket, and every node watches it so
// edits converge cluster-wide without per-call coordination.

const (
	mcpConfigBucketName = "go-api-gateway-mcp-config"
	mcpConfigKey        = "config"
	mcpConfigTTL        = 0
)

// MCPConfig is the wire/JSON shape of the MCP surface allowlist.
type MCPConfig struct {
	AutoInclude bool     `json:"auto_include"`
	Include     []string `json:"include,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
}

// mcpConfigState is the gateway-local mirror — populated from the KV
// watch loop in cluster mode and from putMCPConfig in standalone.
// nil means "no config observed yet", which behaves like a default-zero
// MCPConfig (AutoInclude=false, Include nil → surface empty).
type mcpConfigState struct {
	cfg MCPConfig
}

// mcpConfigSnapshot returns a copy of the gateway-local allowlist.
func (g *Gateway) mcpConfigSnapshot() MCPConfig {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mcpConfigSnapshotLocked()
}

// mcpConfigSnapshotLocked clones g.mcpConfig under g.mu so callers can
// inspect it lock-free.
func (g *Gateway) mcpConfigSnapshotLocked() MCPConfig {
	if g.mcpConfig == nil {
		return MCPConfig{}
	}
	out := MCPConfig{AutoInclude: g.mcpConfig.cfg.AutoInclude}
	if n := len(g.mcpConfig.cfg.Include); n > 0 {
		out.Include = make([]string, n)
		copy(out.Include, g.mcpConfig.cfg.Include)
	}
	if n := len(g.mcpConfig.cfg.Exclude); n > 0 {
		out.Exclude = make([]string, n)
		copy(out.Exclude, g.mcpConfig.cfg.Exclude)
	}
	return out
}

// mcpAllows reports whether the dotted path `p` (e.g.
// "admin.peers.list") should appear on the MCP surface. Internal
// `_*` namespaces are filtered first; then the AutoInclude flag
// chooses between Include-only (default) and exclude-from-all modes.
func (g *Gateway) mcpAllows(p string) bool {
	if p == "" {
		return false
	}
	head := p
	if i := strings.IndexByte(p, '.'); i > 0 {
		head = p[:i]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.isInternal(head) {
		return false
	}
	cfg := g.mcpConfigSnapshotLocked()
	if cfg.AutoInclude {
		for _, pat := range cfg.Exclude {
			if mcpMatch(pat, p) {
				return false
			}
		}
		return true
	}
	for _, pat := range cfg.Include {
		if mcpMatch(pat, p) {
			return true
		}
	}
	return false
}

// setMCPConfig persists the new MCPConfig. In cluster mode it Puts to
// the mcp_config bucket — the watch loop on every node (including
// this one) reflects it back into g.mcpConfig. In standalone mode it
// updates the in-process state directly. Either way, MCPAllows on
// the local gateway reflects the change by the time the next call
// returns (cluster path: best-effort short await on the watch).
func (g *Gateway) setMCPConfig(ctx context.Context, cfg MCPConfig) error {
	if t := g.peers; t != nil && t.mcpConfig != nil {
		raw, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("mcp_config marshal: %w", err)
		}
		if _, err := t.mcpConfig.Put(ctx, mcpConfigKey, raw); err != nil {
			return fmt.Errorf("mcp_config put: %w", err)
		}
		return nil
	}
	g.mu.Lock()
	g.mcpConfig = &mcpConfigState{cfg: cloneMCPConfig(cfg)}
	g.mu.Unlock()
	return nil
}

// mutateMCPConfig applies `mut` to the current MCPConfig and persists
// the result. Standalone mode: the mutation runs under g.mu — no
// concurrent admins. Cluster mode: a CAS loop reads (Get), applies mut,
// and Creates / Updates with the prior revision; concurrent admin
// edits on different gateways converge via retry. Returns the
// post-mutation config so callers don't have to round-trip through
// the watch loop to confirm the change.
func (g *Gateway) mutateMCPConfig(ctx context.Context, mut func(*MCPConfig)) (MCPConfig, error) {
	if t := g.peers; t != nil && t.mcpConfig != nil {
		return clusterMutateMCPConfig(ctx, t.mcpConfig, mut)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := g.mcpConfigSnapshotLocked()
	mut(&cur)
	g.mcpConfig = &mcpConfigState{cfg: cloneMCPConfig(cur)}
	return cloneMCPConfig(cur), nil
}

func clusterMutateMCPConfig(ctx context.Context, kv jetstream.KeyValue, mut func(*MCPConfig)) (MCPConfig, error) {
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		next, err := tryMutateMCPConfig(cctx, kv, mut)
		cancel()
		if err == nil {
			return next, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return MCPConfig{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("mcp_config: CAS retry budget exhausted")
	}
	return MCPConfig{}, fmt.Errorf("mcp_config: %w", lastErr)
}

func tryMutateMCPConfig(ctx context.Context, kv jetstream.KeyValue, mut func(*MCPConfig)) (MCPConfig, error) {
	entry, err := kv.Get(ctx, mcpConfigKey)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return MCPConfig{}, err
	}
	var cur MCPConfig
	var rev uint64
	if entry != nil {
		if uerr := json.Unmarshal(entry.Value(), &cur); uerr != nil {
			// Malformed payload — overwrite below.
			cur = MCPConfig{}
		}
		rev = entry.Revision()
	}
	mut(&cur)
	payload, err := json.Marshal(cur)
	if err != nil {
		return MCPConfig{}, err
	}
	if rev == 0 {
		if _, err := kv.Create(ctx, mcpConfigKey, payload); err != nil {
			return MCPConfig{}, err
		}
	} else {
		if _, err := kv.Update(ctx, mcpConfigKey, payload, rev); err != nil {
			return MCPConfig{}, err
		}
	}
	return cur, nil
}

func cloneMCPConfig(cfg MCPConfig) MCPConfig {
	out := MCPConfig{AutoInclude: cfg.AutoInclude}
	if n := len(cfg.Include); n > 0 {
		out.Include = make([]string, n)
		copy(out.Include, cfg.Include)
	}
	if n := len(cfg.Exclude); n > 0 {
		out.Exclude = make([]string, n)
		copy(out.Exclude, cfg.Exclude)
	}
	return out
}

// mcpMatch applies the per-segment glob. `**` globs any segment count
// (incl. zero); `*` globs one segment; other path.Match metacharacters
// apply within a segment.
func mcpMatch(pattern, p string) bool {
	return globMatchSegments(strings.Split(pattern, "."), strings.Split(p, "."))
}

func globMatchSegments(pats, segs []string) bool {
	for len(pats) > 0 {
		if pats[0] == "**" {
			if len(pats) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if globMatchSegments(pats[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pats[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pats = pats[1:]
		segs = segs[1:]
	}
	return len(segs) == 0
}

// mcpConfigWatchLoop subscribes to the mcp_config bucket and pushes
// every observed value into g.mcpConfig. Mirrors stableWatchLoop:
// jetstream.IncludeHistory() replays the bucket's initial state (one
// Put per key on attach), so a freshly-joining node picks up the
// current cluster config without an extra fetch.
func (t *peerTracker) mcpConfigWatchLoop(ctx context.Context) {
	defer close(t.mcpConfigDone)
	for {
		w, err := t.mcpConfig.WatchAll(ctx, jetstream.IncludeHistory())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if t.gw.cfg.cluster != nil && t.gw.cfg.cluster.Server != nil {
				t.gw.cfg.cluster.Server.Warnf("mcp_config watch: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for ev := range w.Updates() {
			if ev == nil {
				continue
			}
			if ev.Key() != mcpConfigKey {
				continue
			}
			switch ev.Operation() {
			case jetstream.KeyValuePut:
				var v MCPConfig
				if err := json.Unmarshal(ev.Value(), &v); err != nil {
					if t.gw.cfg.cluster != nil && t.gw.cfg.cluster.Server != nil {
						t.gw.cfg.cluster.Server.Warnf("mcp_config watch: bad value: %v", err)
					}
					continue
				}
				t.gw.mu.Lock()
				t.gw.mcpConfig = &mcpConfigState{cfg: cloneMCPConfig(v)}
				t.gw.mu.Unlock()
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				t.gw.mu.Lock()
				t.gw.mcpConfig = nil
				t.gw.mu.Unlock()
			}
		}
		_ = w.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}
