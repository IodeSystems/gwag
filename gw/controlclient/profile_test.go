package controlclient_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "proto/orders.proto", `syntax = "proto3"; package orders.v1; import "common/money.proto";
message O { common.v1.Money m = 1; } service S { rpc M(O) returns (O); }`)
	writeFile(t, dir, "proto/common/money.proto", `syntax = "proto3"; package common.v1; message Money { int64 cents = 1; }`)
	writeFile(t, dir, "api/v2.json", `{"openapi":"3.0.3","info":{"title":"orders","version":"2"},"paths":{}}`)
	writeFile(t, dir, "gwag.yaml", `service: orders
owner: {team: orders, contact: "#orders"}
health: /healthz
provides:
  v1: {kind: proto, file: proto/orders.proto, url: /orders.proto}
  v2: {kind: openapi, file: api/v2.json, url: /v2/openapi, health: /v2/healthz}
  v3: {kind: graphql, endpoint: /graphql}
depends:
  users: [v1]
  library: [v1, v2]
subscribes:
  - {channel: library.v1.BookEvents, group: orders}
`)
	svcs, err := controlclient.LoadProfile(filepath.Join(dir, "gwag.yaml"), "orders:8080")
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 3 {
		t.Fatalf("services=%d, want 3", len(svcs))
	}
	v1, v2, v3 := svcs[0], svcs[1], svcs[2]
	if v1.Version != "v1" || !strings.Contains(string(v1.ProtoSource), "package orders.v1") ||
		len(v1.ProtoImports) != 1 || v1.ProtoImports["common/money.proto"] == nil ||
		v1.HealthURL != "/healthz" || v1.SpecURL != "/orders.proto" {
		t.Fatalf("v1=%+v", v1)
	}
	if v2.Version != "v2" || len(v2.OpenAPISpec) == 0 || v2.HealthURL != "/v2/healthz" || v2.SpecURL != "/v2/openapi" {
		t.Fatalf("v2=%+v", v2)
	}
	if v3.GraphQLEndpoint != "http://orders:8080/graphql" || v3.SpecURL != "" {
		t.Fatalf("v3=%+v", v3)
	}
	for _, s := range svcs {
		if s.Namespace != "orders" || s.Owner == nil || s.Owner.Team != "orders" ||
			len(s.Depends) != 2 || s.Depends[0].Service != "library" || len(s.Subscribes) != 1 {
			t.Fatalf("manifest of %s=%+v", s.Version, s)
		}
	}

	writeFile(t, dir, "bad.yaml", "service: orders\nprovides:\n  v1: {kind: proto}\n")
	if _, err := controlclient.LoadProfile(filepath.Join(dir, "bad.yaml"), ""); err == nil || !strings.Contains(err.Error(), "provides.v1.file") {
		t.Fatalf("err=%v, want the missing file named", err)
	}
	writeFile(t, dir, "typo.yaml", "service: orders\nprovide:\n  v1: {kind: proto}\n")
	if _, err := controlclient.LoadProfile(filepath.Join(dir, "typo.yaml"), ""); err == nil || !strings.Contains(err.Error(), "provide") {
		t.Fatalf("err=%v, want the unknown key refused", err)
	}
}

// SelfRegister with HealthURL / SpecURL blocks while the gateway
// reports the instance red, logs the reason, and returns once green.
func TestSelfRegister_WaitsWhileRed(t *testing.T) {
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(),
		gateway.WithVerification(gateway.VerifyOptions{Interval: 100 * time.Millisecond, Retry: 30 * time.Millisecond}))
	t.Cleanup(g.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveGateway(t, g, lis)

	spec := `{"openapi":"3.0.3","info":{"title":"pets","version":"1"},"paths":{}}`
	var health atomic.Int32
	health.Store(503)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(int(health.Load()))
		case "/openapi.json":
			_, _ = w.Write([]byte(spec))
		}
	}))
	t.Cleanup(svc.Close)

	logs := &logSink{}
	done := make(chan result, 1)
	go func() {
		reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
			GatewayAddr:       lis.Addr().String(),
			ServiceAddr:       svc.URL,
			ReadyPollInterval: 20 * time.Millisecond,
			Logger:            logs.logf,
			Services: []controlclient.Service{{
				Namespace: "pets", Version: "v1", OpenAPISpec: []byte(spec),
				HealthURL: "/healthz", SpecURL: "/openapi.json",
			}},
		})
		done <- result{reg, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("SelfRegister returned while red: err=%v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if !logs.contains("waiting: pets/v1 red: health " + svc.URL + "/healthz: status 503, want 200") {
		t.Fatalf("no red reason in the log: %v", logs.lines)
	}
	health.Store(200)
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SelfRegister did not return once green")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = r.reg.Close(context.Background()) })
	if bs := r.reg.Bindings(); len(bs) != 1 || bs[0].Verification != "green" || bs[0].State != controlclient.StateReady {
		t.Fatalf("bindings=%+v", bs)
	}
	if !logs.contains("ready, starting") {
		t.Fatalf("no ready log: %v", logs.lines)
	}
}
