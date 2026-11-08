package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/controlclient"
	akv1 "github.com/iodesystems/gwag/v2/gw/proto/appkeyauth/v1"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// serveCP serves g's control plane on a real gRPC listener (so calls
// carry a peer, as remote calls do) and returns a client.
func serveCP(t *testing.T, g *Gateway) cpv1.ControlPlaneClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return cpv1.NewControlPlaneClient(conn)
}

func keyCtx(key string) context.Context {
	ctx := context.Background()
	if key == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
}

func mintKey(t *testing.T, g *Gateway, ttl time.Duration, ns ...string) (string, string) {
	t.Helper()
	r, key, err := g.createAppKey(context.Background(), ns, ttl, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	return r.ID, key
}

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("%s: %v (%s), want %s", what, err, got, want)
	}
}

func newKeyGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()
	g := New(append([]Option{WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithRequireAppKeys()}, opts...)...)
	t.Cleanup(g.Close)
	return g
}

func TestAppKeys_ControlPlane(t *testing.T) {
	g := newKeyGateway(t)
	cp := serveCP(t, g)
	greeterID, greeterKey := mintKey(t, g, time.Hour, "greeter")
	_, libraryKey := mintKey(t, g, time.Hour, "library")
	reg := func(key string, b *cpv1.ServiceBinding) (*cpv1.RegisterResponse, error) {
		return cp.Register(keyCtx(key), &cpv1.RegisterRequest{Addr: "127.0.0.1:1", InstanceId: "i-" + b.Namespace, Services: []*cpv1.ServiceBinding{b}})
	}

	_, err := reg("", greeterBinding(t, "v1"))
	wantCode(t, "register without a key", err, codes.Unauthenticated)
	_, err = reg("gwk_0123456789abcdef_"+strings.Repeat("0", 64), greeterBinding(t, "v1"))
	wantCode(t, "register with an unknown key", err, codes.Unauthenticated)
	_, err = reg(g.DevTokenHex(), greeterBinding(t, "v1"))
	wantCode(t, "register with the dev token", err, codes.Unauthenticated)
	_, err = reg(libraryKey, greeterBinding(t, "v1"))
	wantCode(t, "register with another namespace's key", err, codes.PermissionDenied)
	resp, err := reg(greeterKey, greeterBinding(t, "v1"))
	if err != nil {
		t.Fatalf("register with the key: %v", err)
	}
	regID := resp.GetRegistrationId()
	// The admin token bypasses the key check.
	if _, err := reg("tok", libraryBinding(t, "v1")); err != nil {
		t.Fatalf("register with the admin token: %v", err)
	}
	// In-process calls are not checked.
	mustRegister(t, g, "127.0.0.1:2", func() *cpv1.ServiceBinding { b := libraryBinding(t, "v2"); return b }())

	hb := func(key string) (*cpv1.HeartbeatResponse, error) {
		return cp.Heartbeat(keyCtx(key), &cpv1.HeartbeatRequest{RegistrationId: regID})
	}
	_, err = hb("")
	wantCode(t, "heartbeat without a key", err, codes.Unauthenticated)
	_, err = hb(libraryKey)
	wantCode(t, "heartbeat with another namespace's key", err, codes.PermissionDenied)
	if r, err := hb(greeterKey); err != nil || !r.GetOk() {
		t.Fatalf("heartbeat: %v %v", r, err)
	}
	if r, err := cp.Heartbeat(keyCtx(libraryKey), &cpv1.HeartbeatRequest{RegistrationId: "nobody"}); err != nil || r.GetOk() {
		t.Fatalf("heartbeat of an unknown registration with a valid key: %v %v", r, err)
	}

	_, err = cp.DrainInstance(keyCtx(libraryKey), &cpv1.DrainInstanceRequest{Instance: regID})
	wantCode(t, "drain with another namespace's key", err, codes.PermissionDenied)
	if _, err := cp.DrainInstance(keyCtx(greeterKey), &cpv1.DrainInstanceRequest{Instance: "i-greeter", Cancel: true}); err != nil {
		t.Fatalf("drain cancel with the key: %v", err)
	}

	// Operator RPCs need the admin token; reads stay open.
	_, err = cp.ForgetPeer(keyCtx(greeterKey), &cpv1.ForgetPeerRequest{NodeId: "x"})
	wantCode(t, "forget peer with an app key", err, codes.PermissionDenied)
	_, err = cp.Deprecate(keyCtx(""), &cpv1.DeprecateRequest{Namespace: "greeter", Version: "v1", Reason: "x"})
	wantCode(t, "deprecate without a token", err, codes.Unauthenticated)
	if _, err := cp.Deprecate(keyCtx("tok"), &cpv1.DeprecateRequest{Namespace: "greeter", Version: "v1", Reason: "x"}); err != nil {
		t.Fatalf("deprecate with the admin token: %v", err)
	}
	if _, err := cp.ListServices(keyCtx(""), &cpv1.ListServicesRequest{}); err != nil {
		t.Fatalf("list services without a token: %v", err)
	}

	_, err = cp.Deregister(keyCtx(libraryKey), &cpv1.DeregisterRequest{RegistrationId: regID})
	wantCode(t, "deregister with another namespace's key", err, codes.PermissionDenied)

	// Revoked: the heartbeat fails and does not refresh the
	// registration, so it drops at its TTL.
	g.cp.mu.Lock()
	before := g.cp.regs[regID].lastBeatMs.Load()
	g.cp.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if err := g.deleteAppKey(context.Background(), greeterID); err != nil {
		t.Fatal(err)
	}
	_, err = hb(greeterKey)
	wantCode(t, "heartbeat with a revoked key", err, codes.Unauthenticated)
	g.cp.mu.Lock()
	after := g.cp.regs[regID].lastBeatMs.Load()
	g.cp.mu.Unlock()
	if after != before {
		t.Fatalf("a refused heartbeat refreshed the registration")
	}
}

// Without WithRequireAppKeys the control plane is open.
func TestAppKeys_NotRequired(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	cp := serveCP(t, g)
	if _, err := cp.Register(keyCtx(""), &cpv1.RegisterRequest{Addr: "127.0.0.1:1", Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1")}}); err != nil {
		t.Fatal(err)
	}
}

func TestAppKeys_ExpiryAndWarning(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	g := newKeyGateway(t)
	cp := serveCP(t, g)
	id, key := mintKey(t, g, 10*24*time.Hour, "greeter")
	// Two keys may cover one namespace: the replacement is valid
	// while the old one is.
	_, next := mintKey(t, g, 40*24*time.Hour, "greeter")
	resp, err := cp.Register(keyCtx(key), &cpv1.RegisterRequest{Addr: "127.0.0.1:1", Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.warnExpiringAppKeys(context.Background()); len(got) != 0 {
		t.Fatalf("10 days left: warned %v", got)
	}
	now = now.Add(4 * 24 * time.Hour)
	if got := g.warnExpiringAppKeys(context.Background()); len(got) != 1 || got[0] != id {
		t.Fatalf("6 days left: warned %v, want [%s]", got, id)
	}
	if got := g.warnExpiringAppKeys(context.Background()); len(got) != 0 {
		t.Fatalf("warned twice in a day: %v", got)
	}
	hb := func(k string) error {
		_, err := cp.Heartbeat(keyCtx(k), &cpv1.HeartbeatRequest{RegistrationId: resp.GetRegistrationId()})
		return err
	}
	if err := hb(next); err != nil {
		t.Fatalf("heartbeat with the replacement key: %v", err)
	}
	now = now.Add(7 * 24 * time.Hour)
	err = hb(key)
	wantCode(t, "heartbeat with an expired key", err, codes.Unauthenticated)
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired key error: %v", err)
	}
	if err := hb(next); err != nil {
		t.Fatalf("heartbeat with the replacement key after the old one expired: %v", err)
	}
	if got := g.warnExpiringAppKeys(context.Background()); len(got) != 1 || got[0] != id {
		t.Fatalf("expired: warned %v, want [%s]", got, id)
	}
}

// A registration whose key is revoked stops heartbeating and drops at
// its TTL; controlclient sends the key it is given.
func TestAppKeys_SelfRegisterRevokeDropsSlot(t *testing.T) {
	g := newKeyGateway(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	id, key := mintKey(t, g, time.Hour, "greeter")
	_, addr := startGreeterServer(t)
	opts := controlclient.Options{
		GatewayAddr: lis.Addr().String(),
		ServiceAddr: addr,
		TTL:         time.Second,
		Logger:      func(string, ...any) {},
		Services:    []controlclient.Service{{Namespace: "greeter", Version: "v1", ProtoSource: testProtoBytes(t, "greeter.proto")}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := controlclient.SelfRegister(ctx, opts); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("SelfRegister without a key: %v", err)
	}
	opts.AppKey = key
	reg, err := controlclient.SelfRegister(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	waitForSlotTest(t, g, "greeter", "v1", 5*time.Second)
	if err := g.deleteAppKey(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 15*time.Second, "greeter/v1 dropped after revoke", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.slots[poolKey{namespace: "greeter", version: "v1"}] == nil
	})
}

// fakeAppKeyConn answers AppKeyAuthorizer.Authorize in-process.
type fakeAppKeyConn struct {
	handler func(*akv1.AuthorizeRequest) *akv1.AuthorizeResponse
	calls   int
}

func (f *fakeAppKeyConn) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if method != akv1.AppKeyAuthorizer_Authorize_FullMethodName {
		return fmt.Errorf("fakeAppKeyConn: unsupported method %s", method)
	}
	f.calls++
	resp := f.handler(args.(*akv1.AuthorizeRequest))
	out := reply.(*akv1.AuthorizeResponse)
	out.Code, out.Namespaces, out.KeyId, out.ExpiresUnixMs = resp.Code, resp.Namespaces, resp.KeyId, resp.ExpiresUnixMs
	return nil
}

func (f *fakeAppKeyConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("fakeAppKeyConn: streams not supported")
}

// The delegate decides first: OK with its namespaces, DENIED without
// fall-through, UNAVAILABLE falls through to the gateway's keys.
func TestAppKeys_Delegate(t *testing.T) {
	g := newKeyGateway(t)
	cp := serveCP(t, g)
	_, builtin := mintKey(t, g, time.Hour, "greeter")
	fake := &fakeAppKeyConn{handler: func(in *akv1.AuthorizeRequest) *akv1.AuthorizeResponse {
		switch in.GetToken() {
		case "corp-orders":
			return &akv1.AuthorizeResponse{Code: akv1.AppKeyAuthCode_APP_KEY_AUTH_CODE_OK, Namespaces: []string{"greeter"}, KeyId: "corp-1"}
		case "corp-denied":
			return &akv1.AuthorizeResponse{Code: akv1.AppKeyAuthCode_APP_KEY_AUTH_CODE_DENIED}
		}
		return &akv1.AuthorizeResponse{Code: akv1.AppKeyAuthCode_APP_KEY_AUTH_CODE_UNAVAILABLE}
	}}
	src, err := os.ReadFile("proto/appkeyauth/v1/appkeyauth.proto")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.AddProtoBytes("appkeyauth.proto", src, To(fake), As(appKeyAuthorizerNamespace), AsInternal()); err != nil {
		t.Fatal(err)
	}
	reg := func(key string, b *cpv1.ServiceBinding) error {
		_, err := cp.Register(keyCtx(key), &cpv1.RegisterRequest{Addr: "127.0.0.1:1", Services: []*cpv1.ServiceBinding{b}})
		return err
	}
	if err := reg("corp-orders", greeterBinding(t, "v1")); err != nil {
		t.Fatalf("delegate OK: %v", err)
	}
	wantCode(t, "delegate OK, other namespace", reg("corp-orders", libraryBinding(t, "v1")), codes.PermissionDenied)
	wantCode(t, "delegate DENIED", reg("corp-denied", greeterBinding(t, "v2")), codes.Unauthenticated)
	if err := reg(builtin, greeterBinding(t, "v2")); err != nil {
		t.Fatalf("delegate UNAVAILABLE falls through to the gateway's key: %v", err)
	}
	if fake.calls != 4 {
		t.Fatalf("delegate calls = %d, want 4", fake.calls)
	}
}

// The admin API mints (secret once), lists (no secret) and revokes;
// every op needs the admin token.
func TestAppKeys_AdminAPI(t *testing.T) {
	g := newKeyGateway(t)
	code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/app-keys", `{"namespaces":["orders","billing","orders"],"ttl":"30d","note":"orders prod"}`)
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		Key    string
		AppKey appKeyInfo
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, appKeyPrefix+created.AppKey.ID+"_") || strings.Join(created.AppKey.Namespaces, ",") != "billing,orders" || created.AppKey.CreatedBy != "admin" {
		t.Fatalf("created %+v", created)
	}
	if _, err := g.checkBuiltinAppKey(context.Background(), created.Key); err != nil {
		t.Fatalf("minted key does not check: %v", err)
	}
	code, body = adminDoBearer(t, g, "tok", http.MethodGet, "/admin/app-keys", "")
	if code != http.StatusOK || !strings.Contains(body, created.AppKey.ID) || strings.Contains(body, created.Key[len(appKeyPrefix)+17:]) || strings.Contains(body, "hash") {
		t.Fatalf("list: %d %s", code, body)
	}
	for _, bad := range []string{`{"namespaces":[],"ttl":"30d"}`, `{"namespaces":["a"],"ttl":"0d"}`, `{"namespaces":["a.b"],"ttl":"1h"}`} {
		if code, body := adminDoBearer(t, g, "tok", http.MethodPost, "/admin/app-keys", bad); code != http.StatusUnprocessableEntity {
			t.Errorf("create %s: %d %s", bad, code, body)
		}
	}
	if code, _ := adminDoBearer(t, g, g.DevTokenHex(), http.MethodGet, "/admin/app-keys", ""); code != http.StatusForbidden {
		t.Errorf("list as dev: %d", code)
	}
	if code, _ := adminDoBearer(t, g, "tok", http.MethodDelete, "/admin/app-keys/0000000000000000", ""); code != http.StatusNotFound {
		t.Errorf("revoke unknown: %d", code)
	}
	if code, body := adminDoBearer(t, g, "tok", http.MethodDelete, "/admin/app-keys/"+created.AppKey.ID, ""); code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if _, err := g.checkBuiltinAppKey(context.Background(), created.Key); err == nil {
		t.Fatal("revoked key still checks")
	}
}

// Cluster: a key minted on one node works on the other; a revoke on
// one node fails the next heartbeat on the other.
func TestAppKeys_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	for _, n := range []*clusterNode{a, b} {
		n.gw.cfg.adminToken = []byte("tok")
		n.gw.cfg.requireAppKeys = true
	}
	waitForPeers(t, a.gw, b.gw)
	id, key := mintKey(t, a.gw, time.Hour, "greeter")

	conn, err := grpc.NewClient(b.cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	cp := cpv1.NewControlPlaneClient(conn)
	_, addr := startGreeterServer(t)
	var resp *cpv1.RegisterResponse
	waitUntilErr(t, 20*time.Second, "register on B with A's key", func() error {
		var err error
		resp, err = cp.Register(keyCtx(key), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{greeterBinding(t, "v1")}})
		return err
	})
	waitForSlotTest(t, a.gw, "greeter", "v1", 10*time.Second)
	if keys, err := b.gw.listAppKeys(context.Background()); err != nil || len(keys) != 1 || keys[0].ID != id {
		t.Fatalf("list on B: %v %v", keys, err)
	}
	if err := a.gw.deleteAppKey(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	_, err = cp.Heartbeat(keyCtx(key), &cpv1.HeartbeatRequest{RegistrationId: resp.GetRegistrationId()})
	wantCode(t, "heartbeat on B after revoke on A", err, codes.Unauthenticated)
}

func TestParseAppKeyTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "720h": 720 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := ParseAppKeyTTL(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0d", "-1h", "x", "1.5d"} {
		if _, err := ParseAppKeyTTL(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}
