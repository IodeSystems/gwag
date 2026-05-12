package gateway

import (
	"context"
	"testing"
	"time"
)

func TestMCPMatch_Glob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"admin.peers.list", "admin.peers.list", true},
		{"admin.peers.list", "admin.peers.forget", false},
		{"admin.peers.*", "admin.peers.list", true},
		{"admin.peers.*", "admin.peers", false},
		{"admin.peers.*", "admin.peers.deep.nested", false},
		{"admin.*", "admin.peers", true},
		{"admin.**", "admin.peers", true},
		{"admin.**", "admin.peers.list", true},
		{"admin.**", "admin", true}, // ** matches zero or more segments
		{"admin.**", "users", false},
		{"**", "anything.at.all", true},
		{"**", "single", true},
		{"*.list*", "users.list", true},
		{"*.list*", "users.listAll", true},
		{"*.list*", "users.create", false},
	}
	for _, c := range cases {
		if got := mcpMatch(c.pattern, c.path); got != c.want {
			t.Errorf("mcpMatch(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestMCPConfig_StandaloneAllowsDefaultDeny(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	defer gw.Close()

	// Empty config: nothing on the surface.
	if gw.MCPAllows("admin.peers.list") {
		t.Fatal("empty config should default-deny")
	}

	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"admin.peers.*", "users.list"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{"admin.peers.list", true},
		{"admin.peers.forget", true},
		{"admin.peers", false},
		{"admin.services.list", false},
		{"users.list", true},
		{"users.create", false},
	}
	for _, c := range cases {
		if got := gw.MCPAllows(c.path); got != c.want {
			t.Errorf("MCPAllows(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMCPConfig_AutoIncludeWithExclude(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	defer gw.Close()

	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		AutoInclude: true,
		Exclude:     []string{"admin.**", "*.delete"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}

	cases := []struct {
		path string
		want bool
	}{
		{"users.list", true},
		{"users.delete", false},
		{"admin.peers.list", false},
		{"admin.users.list", false},
	}
	for _, c := range cases {
		if got := gw.MCPAllows(c.path); got != c.want {
			t.Errorf("MCPAllows(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMCPConfig_InternalNSFilteredFirst(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	defer gw.Close()

	// Even in AutoInclude=true mode with `**` not explicitly excluded,
	// `_*` namespaces stay hidden.
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		AutoInclude: true,
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	if gw.MCPAllows("_admin_auth.foo") {
		t.Error("internal _admin_auth should be filtered first")
	}

	// And in Include mode, an Include pattern can't override the
	// internal filter.
	if err := gw.SetMCPConfig(context.Background(), MCPConfig{
		Include: []string{"_admin_events.**"},
	}); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	if gw.MCPAllows("_admin_events.publish") {
		t.Error("Include should not override internal-NS filter")
	}
}

func TestMCPConfig_Snapshot(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	defer gw.Close()

	if snap := gw.MCPConfigSnapshot(); snap.AutoInclude || len(snap.Include) != 0 || len(snap.Exclude) != 0 {
		t.Fatalf("default snapshot non-empty: %+v", snap)
	}

	in := MCPConfig{
		AutoInclude: true,
		Include:     []string{"a.b"},
		Exclude:     []string{"c.d"},
	}
	if err := gw.SetMCPConfig(context.Background(), in); err != nil {
		t.Fatalf("SetMCPConfig: %v", err)
	}
	snap := gw.MCPConfigSnapshot()
	if !snap.AutoInclude || len(snap.Include) != 1 || snap.Include[0] != "a.b" || len(snap.Exclude) != 1 || snap.Exclude[0] != "c.d" {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}

	// Mutating the snapshot must not mutate the gateway's state.
	snap.Include[0] = "MUTATED"
	snap2 := gw.MCPConfigSnapshot()
	if snap2.Include[0] != "a.b" {
		t.Fatalf("snapshot is not a deep copy: got %q", snap2.Include[0])
	}
}

// TestMCPConfig_ClusterConverges writes a config on node A and waits for
// node B's local state to converge via the watch loop. Mirrors the
// stable-cluster convergence pattern (jetstream.IncludeHistory replays
// the bucket's current state to a fresh watcher).
func TestMCPConfig_ClusterConverges(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)

	cfg := MCPConfig{
		AutoInclude: false,
		Include:     []string{"admin.peers.*", "library.book.list"},
	}

	seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer seedCancel()
	var seedErr error
	for seedCtx.Err() == nil {
		ctx, cancel := context.WithTimeout(seedCtx, 2*time.Second)
		seedErr = a.gw.SetMCPConfig(ctx, cfg)
		cancel()
		if seedErr == nil {
			break
		}
		select {
		case <-seedCtx.Done():
			break
		case <-time.After(100 * time.Millisecond):
		}
	}
	if seedErr != nil {
		t.Fatalf("A.SetMCPConfig: %v", seedErr)
	}

	convergeCtx, convergeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer convergeCancel()
	for convergeCtx.Err() == nil {
		snap := b.gw.MCPConfigSnapshot()
		if len(snap.Include) == 2 && snap.Include[0] == "admin.peers.*" && snap.Include[1] == "library.book.list" {
			if !b.gw.MCPAllows("admin.peers.list") {
				t.Fatalf("B.MCPAllows(admin.peers.list) should be true after convergence: %+v", snap)
			}
			if b.gw.MCPAllows("admin.services.list") {
				t.Fatalf("B.MCPAllows(admin.services.list) should be false")
			}
			return
		}
		select {
		case <-convergeCtx.Done():
			break
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("B did not converge on A's MCPConfig: B snapshot=%+v", b.gw.MCPConfigSnapshot())
}
