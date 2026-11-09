package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// System keys (system_keys.go), the cutover freeze
// (cluster_freeze.go) and the cluster state export and import
// (cluster_state.go).

func newCutoverGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	g := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithReplayVerifyEndpoint(), WithSchemaRegistry(SchemaRegistryOptions{Writes: true})}, opts...)...)
	t.Cleanup(g.Close)
	return g
}

func mintSystemKey(t *testing.T, g *Gateway, ttl time.Duration) (string, string) {
	t.Helper()
	r, key, err := g.createSystemKey(context.Background(), ttl, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	return r.ID, key
}

func TestSystemKeys_Scope(t *testing.T) {
	g := newCutoverGateway(t, WithRequireAppKeys())
	id, sys := mintSystemKey(t, g, time.Hour)
	_, app := mintKey(t, g, time.Hour, "greeter")
	if !strings.HasPrefix(sys, systemKeyPrefix+id+"_") {
		t.Fatalf("system key %q", sys)
	}

	allowed := func(what string, code int, body string) {
		t.Helper()
		if code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("%s: %d, want allowed (%s)", what, code, body)
		}
	}
	code, body := adminDoBearer(t, g, sys, http.MethodPost, "/admin/replay/verify", `{}`)
	allowed("verify with the system key", code, body)
	if code != http.StatusOK || !strings.Contains(body, `"outcome"`) {
		t.Errorf("verify: %d %s", code, body)
	}
	code, body = adminDoBearer(t, g, sys, http.MethodGet, "/admin/cluster/state?allowUnfrozen=true", "")
	if code != http.StatusOK || !strings.Contains(body, `"v":1`) {
		t.Errorf("export with the system key: %d %s", code, body)
	}
	code, body = adminDoBearer(t, g, sys, http.MethodPost, "/admin/cluster/state", `{"state":{"v":1,"stable":[],"deprecated":[],"keys":[],"externalDependents":[]},"dryRun":true}`)
	if code != http.StatusOK {
		t.Errorf("import with the system key: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/admin/replay/diffs", ""},
		{http.MethodGet, "/admin/app-keys", ""},
		{http.MethodPost, "/admin/app-keys", `{"namespaces":["x"],"ttl":"1d"}`},
		{http.MethodPost, "/admin/system-keys", `{"ttl":"1d"}`},
		{http.MethodPost, "/admin/cluster/freeze", `{}`},
		{http.MethodGet, "/admin/services", ""},
		{http.MethodGet, "/admin/whoami", ""},
	} {
		if code, body := adminDoBearer(t, g, sys, c.method, c.path, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s with the system key: %d, want 403 (%s)", c.method, c.path, code, body)
		}
	}

	// An app key is not a system key, nor the admin token.
	if code, _ := adminDoBearer(t, g, app, http.MethodPost, "/admin/replay/verify", `{}`); code != http.StatusUnauthorized {
		t.Errorf("verify with an app key: %d, want 401", code)
	}
	if code, _ := adminDoBearer(t, g, app, http.MethodGet, "/admin/cluster/state?allowUnfrozen=true", ""); code != http.StatusUnauthorized {
		t.Errorf("export with an app key: %d, want 401", code)
	}
	// The system key is not an app key: control plane and registry.
	cp := serveCP(t, g)
	_, err := cp.Register(keyCtx(sys), &cpv1.RegisterRequest{Addr: "127.0.0.1:1", Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1")}})
	wantCode(t, "register with the system key", err, codes.Unauthenticated)
	_, err = cp.Deprecate(keyCtx(sys), &cpv1.DeprecateRequest{Namespace: "greeter", Version: "v1", Reason: "x"})
	wantCode(t, "deprecate with the system key", err, codes.PermissionDenied)
	c := srClient(t, srServer(t, g), sr.Config{Token: sys})
	if _, err := c.Register(context.Background(), "x-value", sr.Schema{Schema: `syntax = "proto3"; message X {}`}, false); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("schema registry write with the system key: %v, want 401", err)
	}
	// Same id and secret under the app-key prefix: not a key.
	if _, err := g.checkBuiltinAppKey(context.Background(), appKeyPrefix+strings.TrimPrefix(sys, systemKeyPrefix)); err == nil {
		t.Error("system key accepted as an app key")
	}

	// Expired, then revoked.
	old := nowFunc
	nowFunc = func() time.Time { return old().Add(2 * time.Hour) }
	code, _ = adminDoBearer(t, g, sys, http.MethodPost, "/admin/replay/verify", `{}`)
	nowFunc = old
	if code != http.StatusUnauthorized {
		t.Errorf("verify with an expired system key: %d, want 401", code)
	}
	code, body = adminDoBearer(t, g, "tok", http.MethodGet, "/admin/app-keys", "")
	if code != http.StatusOK || !strings.Contains(body, `"kind":"system"`) || !strings.Contains(body, `"kind":"app"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body := adminDoBearer(t, g, "tok", http.MethodDelete, "/admin/app-keys/"+id, ""); code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := adminDoBearer(t, g, sys, http.MethodPost, "/admin/replay/verify", `{}`); code != http.StatusUnauthorized {
		t.Errorf("verify with a revoked system key: %d, want 401", code)
	}
}

// freezeWrites is every frozen write kind as an admin request
// (admin token "tok").
var freezeWrites = []struct{ name, method, path, body string }{
	{"retractStable", http.MethodPost, "/admin/services/greeter/stable/retract", `{"targetVN":1}`},
	{"deprecate", http.MethodPost, "/admin/services/greeter/v1/deprecate", `{"reason":"x"}`},
	{"undeprecate", http.MethodPost, "/admin/services/greeter/v1/undeprecate", ""},
	{"createAppKey", http.MethodPost, "/admin/app-keys", `{"namespaces":["greeter"],"ttl":"1d"}`},
	{"createSystemKey", http.MethodPost, "/admin/system-keys", `{"ttl":"1d"}`},
	{"revokeAppKey", http.MethodDelete, "/admin/app-keys/0000000000000000", ""},
	{"putExternalDependent", http.MethodPut, "/admin/external-dependents/zeta", acmeV1},
	{"reviewExternalDependent", http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2099-01-01T00:00:00Z"}`},
	{"deleteExternalDependent", http.MethodDelete, "/admin/external-dependents/acme", ""},
}

// checkFrozenWrites asserts every write kind is refused on g with 409
// naming the freeze, the control-plane Deprecate with
// FailedPrecondition, and a Schema Registry register of a new schema
// with 409; a register of a schema the subject holds still answers.
func checkFrozenWrites(t *testing.T, g *Gateway, cp cpv1.ControlPlaneClient, srURL string) {
	t.Helper()
	for _, w := range freezeWrites {
		code, body := adminDoBearer(t, g, "tok", w.method, w.path, w.body)
		if code != http.StatusConflict || !strings.Contains(body, "cluster frozen for cutover until") {
			t.Errorf("%s while frozen: %d %s, want 409", w.name, code, body)
		}
	}
	_, err := cp.Deprecate(context.Background(), &cpv1.DeprecateRequest{Namespace: "greeter", Version: "v1", Reason: "x"})
	wantCode(t, "control-plane deprecate while frozen", err, codes.FailedPrecondition)
	c := srClient(t, srURL, sr.Config{Token: "tok"})
	ctx := context.Background()
	if _, err := c.Register(ctx, "frozen-value", sr.Schema{Schema: `syntax = "proto3"; package f; message New {}`}, false); err == nil || !strings.Contains(err.Error(), "40903") {
		t.Errorf("registry register while frozen: %v, want 40903", err)
	}
	id, err := c.Register(ctx, "kept-value", sr.Schema{Schema: `syntax = "proto3"; package k; message Kept {}`}, false)
	if err != nil {
		t.Errorf("registry register of a held schema while frozen: %v", err)
	} else if id < 1 {
		t.Errorf("registry register of a held schema: id %d", id)
	}
}

// checkWritesOpen asserts the freeze no longer refuses.
func checkWritesOpen(t *testing.T, g *Gateway) {
	t.Helper()
	code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/system-keys", `{"ttl":"1d"}`)
	if code != http.StatusOK {
		t.Errorf("create a system key after the freeze: %d %s", code, body)
	}
}

func TestClusterFreeze_Standalone(t *testing.T) {
	g := newCutoverGateway(t)
	_, addr := startGreeterServer(t)
	cp := serveCP(t, g)
	if _, err := cp.Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1"), greeterBinding(t, "v2")}}); err != nil {
		t.Fatal(err)
	}
	putExternal(t, g, "acme", acmeV1)
	srURL := srServer(t, g)
	if _, err := srClient(t, srURL, sr.Config{Token: "tok"}).Register(context.Background(), "kept-value", sr.Schema{Schema: `syntax = "proto3"; package k; message Kept {}`}, false); err != nil {
		t.Fatal(err)
	}

	if code, body := adminDoBearer(t, g, "tok", http.MethodGet, "/admin/cluster/state", ""); code != http.StatusConflict || !strings.Contains(body, "not frozen") {
		t.Fatalf("export unfrozen: %d %s", code, body)
	}
	code, body := adminDoBearer(t, g, "tok", http.MethodGet, "/admin/cluster/state?allowUnfrozen=true", "")
	if code != http.StatusOK || !strings.Contains(body, "exported without a freeze") {
		t.Fatalf("export with allowUnfrozen: %d %s", code, body)
	}
	if code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/cluster/freeze", `{"ttl":"10m","note":"cutover"}`); code != http.StatusOK || !strings.Contains(body, `"frozen":true`) || !strings.Contains(body, `"setBy":"admin"`) {
		t.Fatalf("freeze: %d %s", code, body)
	}
	if code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/cluster/freeze", `{"ttl":"25h"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("freeze for 25h: %d %s", code, body)
	}
	checkFrozenWrites(t, g, cp, srURL)

	// Registrations and heartbeats go on.
	resp, err := cp.Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{libraryBinding(t, "v1")}})
	if err != nil {
		t.Fatalf("register while frozen: %v", err)
	}
	if _, err := cp.Heartbeat(context.Background(), &cpv1.HeartbeatRequest{RegistrationId: resp.GetRegistrationId()}); err != nil {
		t.Fatalf("heartbeat while frozen: %v", err)
	}
	code, body = adminDoBearer(t, g, "tok", http.MethodGet, "/admin/cluster/state", "")
	if code != http.StatusOK || !strings.Contains(body, `"frozen":true`) || strings.Contains(body, "warnings") {
		t.Fatalf("export frozen: %d %s", code, body)
	}

	// The freeze ends by itself at its TTL.
	old := nowFunc
	nowFunc = func() time.Time { return old().Add(11 * time.Minute) }
	checkWritesOpen(t, g)
	if code, body := adminDoBearer(t, g, "tok", http.MethodGet, "/admin/cluster/freeze", ""); code != http.StatusOK || strings.Contains(body, `"frozen":true`) {
		t.Errorf("freeze state after the TTL: %d %s", code, body)
	}
	nowFunc = old

	// Unfreeze ends it at once.
	adminDoBearer(t, g, "tok", http.MethodPost, "/admin/cluster/freeze", `{}`)
	if code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/cluster/unfreeze", ""); code != http.StatusOK || !strings.Contains(body, `"wasFrozen":true`) {
		t.Fatalf("unfreeze: %d %s", code, body)
	}
	checkWritesOpen(t, g)
}

// A freeze set on node A refuses the writes on node B; it ends at its
// TTL on B too.
func TestClusterFreeze_OtherNode(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	opts := []Option{WithAdminToken([]byte("tok")), WithSchemaRegistry(SchemaRegistryOptions{Writes: true})}
	a := newSharedClusterNode(t, testCluster.a, opts...)
	b := newSharedClusterNode(t, testCluster.b, opts...)
	waitForPeers(t, a.gw, b.gw)

	_, addr := startGreeterServer(t)
	cpB := cpClient(t, b.cpAddr)
	waitUntilErr(t, 20*time.Second, "register on B", func() error {
		_, err := cpB.Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1"), greeterBinding(t, "v2")}})
		return err
	})
	putExternal(t, a.gw, "acme", acmeV1)
	waitUntilErr(t, 10*time.Second, "acme on B", func() error {
		if _, ok := b.gw.externals.get("acme"); !ok {
			return errNotYet
		}
		return nil
	})
	srURL := srServer(t, b.gw)
	waitUntilErr(t, 20*time.Second, "registry on B", func() error {
		_, err := srClient(t, srURL, sr.Config{Token: "tok"}).Register(context.Background(), "kept-value", sr.Schema{Schema: `syntax = "proto3"; package k; message Kept {}`}, false)
		return err
	})

	if code, body := adminDoBearer(t, a.gw, "tok", http.MethodPost, "/admin/cluster/freeze", `{"ttl":"3s"}`); code != http.StatusOK {
		t.Fatalf("freeze on A: %d %s", code, body)
	}
	checkFrozenWrites(t, b.gw, cpB, srURL)
	if code, body := adminDoBearer(t, b.gw, "tok", http.MethodGet, "/admin/cluster/freeze", ""); code != http.StatusOK || !strings.Contains(body, `"frozen":true`) {
		t.Fatalf("freeze state on B: %d %s", code, body)
	}
	time.Sleep(3500 * time.Millisecond)
	checkWritesOpen(t, b.gw)
}

var errNotYet = errorString("not yet")

type errorString string

func (e errorString) Error() string { return string(e) }

func cpClient(t *testing.T, addr string) cpv1.ControlPlaneClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return cpv1.NewControlPlaneClient(conn)
}

// startGreenCluster starts a one-node cluster (its own NATS and
// JetStream) with a gateway: the green side of a cutover.
func startGreenCluster(t *testing.T, opts ...Option) *clusterNode {
	t.Helper()
	cl, err := StartCluster(ClusterOptions{
		NodeName:      "green",
		ClientListen:  freeAddr(t),
		ClusterListen: freeAddr(t),
		DataDir:       t.TempDir(),
		StartTimeout:  15 * time.Second,
		LogLevel:      "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster green: %v", err)
	}
	t.Cleanup(cl.Close)
	jctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.WaitForJetStream(jctx); err != nil {
		t.Fatal(err)
	}
	g := New(append([]Option{WithCluster(cl), WithoutMetrics(), WithoutBackpressure()}, opts...)...)
	t.Cleanup(g.Close)
	cpAddr := freeAddr(t)
	lis, err := net.Listen("tcp", cpAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := g.startClusterTracking(context.Background()); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("green startClusterTracking: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &clusterNode{cluster: cl, gw: g, cpAddr: cpAddr, grpcSrv: srv}
}

// registryView is every context's subject versions and their ids.
func registryView(t *testing.T, g *Gateway) map[string]string {
	t.Helper()
	st, err := g.srStore()
	if err != nil || st == nil {
		t.Fatalf("registry store: %v", err)
	}
	ctx := context.Background()
	out := map[string]string{}
	names, err := st.Contexts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		c := st.Context(n)
		b, err := c.Bundle(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range b.Versions {
			sch := ""
			for _, s := range b.Schemas {
				if s.ID == v.ID {
					h := sha256.Sum256([]byte(s.Schema))
					sch = hex.EncodeToString(h[:6])
				}
			}
			out[n+" "+v.Subject+" v"+strconv.Itoa(v.Version)] = strconv.Itoa(v.ID) + " " + sch
		}
	}
	return out
}

// Blue (the shared two-node cluster) to green (its own one-node
// cluster): export on blue under a freeze, import on green. The same
// app key works on green, blue's system key is accepted by green's
// verify, schema ids are equal in the default context and in a named
// one, stable and deprecated marks are equal. A second import changes
// nothing. A service registers with both clusters with two
// controlclient registrations.
func TestClusterState_RoundTrip(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	opts := []Option{WithAdminToken([]byte("tok")), WithSchemaRegistry(SchemaRegistryOptions{Writes: true}), WithReplayVerifyEndpoint()}
	a := newSharedClusterNode(t, testCluster.a, opts...)
	b := newSharedClusterNode(t, testCluster.b, opts...)
	waitForPeers(t, a.gw, b.gw)
	green := startGreenCluster(t, append([]Option{WithRequireAppKeys()}, opts...)...)

	// One service, two registrations: one per cluster.
	_, addr := startGreeterServer(t)
	for _, gwAddr := range []string{a.cpAddr, green.cpAddr} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
			GatewayAddr: gwAddr,
			ServiceAddr: addr,
			InstanceID:  "greeter-0",
			AppKey:      "", // green requires one; set below for green
			Services: []controlclient.Service{
				{Namespace: "greeter", Version: "v1", ProtoSource: testProtoBytes(t, "greeter.proto")},
				{Namespace: "greeter", Version: "v2", ProtoSource: testProtoBytes(t, "greeter.proto")},
			},
			HeartbeatInterval: 500 * time.Millisecond,
		})
		cancel()
		if gwAddr == green.cpAddr {
			// Green requires app keys and has none yet: the
			// registration waits for the import of blue's key.
			if err == nil {
				t.Fatal("register on green without a key: accepted")
			}
			continue
		}
		if err != nil {
			t.Fatalf("SelfRegister on blue: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close(context.Background()) })
	}
	waitForSlotTest(t, b.gw, "greeter", "v2", 15*time.Second)
	cpA := cpClient(t, a.cpAddr)
	if _, err := cpA.Deprecate(context.Background(), &cpv1.DeprecateRequest{Namespace: "greeter", Version: "v1", Reason: "use v2"}); err != nil {
		t.Fatal(err)
	}
	_, appKey := mintKey(t, a.gw, time.Hour, "greeter")
	_, sysKey := mintSystemKey(t, b.gw, time.Hour)
	putExternal(t, a.gw, "acme", acmeV1)

	// Registry: a REST register, then channel subjects (default
	// context), and a named context imported from another registry.
	// The REST register comes first, so blue's channel ids are not the
	// ones green would allocate by itself.
	blueSR := srClient(t, srServer(t, a.gw), sr.Config{Token: "tok"})
	waitUntilErr(t, 20*time.Second, "blue registry", func() error {
		_, err := blueSR.Register(context.Background(), "extra-value", sr.Schema{Schema: `syntax = "proto3"; package extra; message E {}`}, false)
		return err
	})
	addOrdersProto(t, a.gw)
	waitSubject(t, blueSR, "EV_orders_v1_OrderEvents-value")
	st, _ := a.gw.srStore()
	if _, err := st.Context(".corp").Import(context.Background(), srserve.Bundle{
		Schemas:  []srserve.ImportSchema{{ID: 41, SchemaType: "PROTOBUF", Schema: `syntax = "proto3"; package corp; message C {}`}},
		Versions: []srserve.ImportVersion{{Subject: "corp-c-value", Version: 3, ID: 41}},
	}); err != nil {
		t.Fatal(err)
	}

	// Export: refused unfrozen, then frozen, with the system key.
	if code, _ := adminDoBearer(t, b.gw, sysKey, http.MethodGet, "/admin/cluster/state", ""); code != http.StatusConflict {
		t.Fatalf("export unfrozen: %d", code)
	}
	if code, body := adminDoBearer(t, a.gw, "tok", http.MethodPost, "/admin/cluster/freeze", `{"ttl":"5m"}`); code != http.StatusOK {
		t.Fatalf("freeze: %d %s", code, body)
	}
	var doc clusterState
	waitUntilErr(t, 15*time.Second, "export with every mark", func() error {
		code, body := adminDoBearer(t, b.gw, sysKey, http.MethodGet, "/admin/cluster/state", "")
		if code != http.StatusOK {
			return errorString(body)
		}
		doc = clusterState{}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return err
		}
		greeter := false
		for _, m := range doc.Stable {
			greeter = greeter || m == (cutoverStable{"greeter", 2})
		}
		if !greeter || len(doc.Deprecated) != 1 || len(doc.Keys) != 2 || len(doc.ExternalDependents) != 1 {
			return errorString("incomplete: " + body)
		}
		return nil
	})
	if !doc.Frozen || doc.SchemaRegistry == nil || doc.Deprecated[0].Reason != "use v2" || doc.ExportedBy != "system:"+strings.SplitN(sysKey, "_", 3)[1] {
		t.Fatalf("doc: %+v", doc)
	}
	for _, c := range doc.SchemaRegistry.Contexts {
		for _, v := range c.Versions {
			if v.Subject == ControlPlaneSubject {
				t.Fatalf("control plane subject exported: %+v", v)
			}
		}
	}

	// Import on green with green's admin token.
	body, _ := json.Marshal(map[string]any{"state": doc})
	code, out := adminDoBearer(t, green.gw, "tok", http.MethodPost, "/admin/cluster/state", string(body))
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, out)
	}
	var res importResult
	_ = json.Unmarshal([]byte(out), &res)
	if res.Stable.Added != len(doc.Stable) || res.Deprecated.Added != 1 || res.Keys.Added != 2 || res.ExternalDependents.Added != 1 || res.SchemaRegistry.SchemasAdded == 0 {
		t.Fatalf("import result: %s", out)
	}

	// The same app key works on green: the waiting registration.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddr: green.cpAddr, ServiceAddr: addr, InstanceID: "greeter-0", AppKey: appKey,
		Services: []controlclient.Service{
			{Namespace: "greeter", Version: "v1", ProtoSource: testProtoBytes(t, "greeter.proto")},
			{Namespace: "greeter", Version: "v2", ProtoSource: testProtoBytes(t, "greeter.proto")},
		},
		HeartbeatInterval: 500 * time.Millisecond,
	})
	cancel()
	if err != nil {
		t.Fatalf("SelfRegister on green with blue's app key: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	waitForSlotTest(t, green.gw, "greeter", "v2", 15*time.Second)
	// Blue's system key on green's verify.
	if code, body := adminDoBearer(t, green.gw, sysKey, http.MethodPost, "/admin/replay/verify", `{}`); code != http.StatusOK {
		t.Fatalf("green verify with blue's system key: %d %s", code, body)
	}

	// Marks.
	gs, err := green.gw.readCopyState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gs.stable["greeter"] != 2 || gs.deprecated[deprecatedKey("greeter", "v1")] != "use v2" || len(gs.keys) != 2 {
		t.Fatalf("green state: %+v", gs)
	}
	green.gw.mu.Lock()
	reason := green.gw.slots[poolKey{namespace: "greeter", version: "v1"}].deprecationReason
	green.gw.mu.Unlock()
	if reason != "use v2" {
		t.Fatalf("green greeter/v1 deprecation %q", reason)
	}

	// Registry: ids equal; green's own channel registration (orders)
	// finds blue's ids and adds nothing.
	addOrdersProto(t, green.gw)
	greenSR := srClient(t, srServer(t, green.gw), sr.Config{Token: "tok"})
	waitSubject(t, greenSR, "EV_orders_v1_OrderEvents-value")
	waitUntilErr(t, 20*time.Second, "green registers its channels", func() error {
		green.gw.sr.mu.Lock()
		defer green.gw.sr.mu.Unlock()
		if green.gw.sr.done["EV_orders_v1_OrderEvents-value"] == "" {
			return errNotYet
		}
		return nil
	})
	bv, gv := registryView(t, a.gw), registryView(t, green.gw)
	for k, v := range bv {
		if strings.Contains(k, ControlPlaneSubject) {
			continue
		}
		if gv[k] != v {
			t.Errorf("registry %s: blue %q, green %q", k, v, gv[k])
		}
	}
	if gv[".corp corp-c-value v3"] == "" || !strings.HasPrefix(gv[".corp corp-c-value v3"], "41 ") {
		t.Errorf("named context on green: %v", gv)
	}
	for k := range gv {
		if _, ok := bv[k]; !ok && !strings.Contains(k, ControlPlaneSubject) {
			t.Errorf("green has %s that blue has not", k)
		}
	}

	// Idempotent.
	code, out = adminDoBearer(t, green.gw, sysKey, http.MethodPost, "/admin/cluster/state", string(body))
	_ = json.Unmarshal([]byte(out), &res)
	if code != http.StatusOK || res.Stable.Added+res.Deprecated.Added+res.Keys.Added+res.ExternalDependents.Added+res.SchemaRegistry.SchemasAdded+res.SchemaRegistry.VersionsAdded != 0 || len(res.Conflicts) != 0 {
		t.Fatalf("second import: %d %s", code, out)
	}
}

// Import conflicts: an item held with another value fails the import
// and writes nothing; overwrite takes the document's value. A Schema
// Registry id green allocated itself cannot be overwritten.
func TestClusterState_ImportConflicts(t *testing.T) {
	blue := newCutoverGateway(t)
	green := newCutoverGateway(t)
	putExternal(t, blue, "acme", acmeV1)
	putExternal(t, green, "acme", strings.Replace(acmeV1, "contract 2026-14", "other", 1))
	_, key := mintKey(t, blue, time.Hour, "greeter")
	blue.mu.Lock()
	blue.retractStableLocked("greeter", 3)
	blue.setDeprecationLocked("greeter", "v1", "old")
	blue.mu.Unlock()
	green.mu.Lock()
	green.retractStableLocked("greeter", 2)
	green.mu.Unlock()

	adminDoBearer(t, blue, "tok", http.MethodPost, "/admin/cluster/freeze", `{}`)
	_, exported := adminDoBearer(t, blue, "tok", http.MethodGet, "/admin/cluster/state", "")
	var doc clusterState
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	body := func(extra string) string {
		b, _ := json.Marshal(map[string]any{"state": doc})
		return strings.TrimSuffix(string(b), "}") + extra + "}"
	}

	code, out := adminDoBearer(t, green, "tok", http.MethodPost, "/admin/cluster/state", body(""))
	if code != http.StatusConflict || !strings.Contains(out, "external dependent acme differs here") || !strings.Contains(out, "stable greeter is v2 here, v3 in the document") {
		t.Fatalf("conflicting import: %d %s", code, out)
	}
	if _, err := green.checkBuiltinAppKey(context.Background(), key); err == nil {
		t.Fatal("conflicting import wrote the app key")
	}
	code, out = adminDoBearer(t, green, "tok", http.MethodPost, "/admin/cluster/state", body(`,"dryRun":true`))
	if code != http.StatusOK || !strings.Contains(out, `"dryRun":true`) || !strings.Contains(out, "external dependent acme") {
		t.Fatalf("dry run: %d %s", code, out)
	}
	code, out = adminDoBearer(t, green, "tok", http.MethodPost, "/admin/cluster/state", body(`,"overwrite":true`))
	if code != http.StatusOK || !strings.Contains(out, `"overwritten":1`) {
		t.Fatalf("overwrite: %d %s", code, out)
	}
	if e, _ := green.externals.get("acme"); e.Note != "contract 2026-14" {
		t.Errorf("acme on green after overwrite: %+v", e)
	}
	green.mu.Lock()
	vN, reason := green.stableVN["greeter"], green.deprecation[poolKey{namespace: "greeter", version: "v1"}]
	green.mu.Unlock()
	if vN != 3 || reason != "old" {
		t.Errorf("green stable=%d deprecation=%q", vN, reason)
	}
	if _, err := green.checkBuiltinAppKey(context.Background(), key); err != nil {
		t.Errorf("blue's app key on green: %v", err)
	}

	// Registry: green allocates an id blue holds for another schema.
	blueSR := srClient(t, srServer(t, blue), sr.Config{Token: "tok"})
	greenSR := srClient(t, srServer(t, green), sr.Config{Token: "tok"})
	ctx := context.Background()
	adminDoBearer(t, blue, "tok", http.MethodPost, "/admin/cluster/unfreeze", "")
	if _, err := blueSR.Register(ctx, "a-value", sr.Schema{Schema: `syntax = "proto3"; package a; message A {}`}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := greenSR.Register(ctx, "b-value", sr.Schema{Schema: `syntax = "proto3"; package b; message B {}`}, false); err != nil {
		t.Fatal(err)
	}
	_, exported = adminDoBearer(t, blue, "tok", http.MethodGet, "/admin/cluster/state?allowUnfrozen=true", "")
	doc = clusterState{}
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	code, out = adminDoBearer(t, green, "tok", http.MethodPost, "/admin/cluster/state", body(`,"overwrite":true`))
	if code != http.StatusConflict || !strings.Contains(out, "schema registry context .") || !strings.Contains(out, "cannot be overwritten") {
		t.Fatalf("registry conflict with overwrite: %d %s", code, out)
	}
	if _, err := greenSR.SubjectVersion(ctx, "a-value", "latest", ""); err == nil {
		t.Error("conflicting import wrote a-value on green")
	}
}
