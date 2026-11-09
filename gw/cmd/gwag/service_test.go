package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"google.golang.org/grpc"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/multi/gen/greeter/v1"
	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/profile"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

func runSvc(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runService(args, &out, &errb)
	return code, out.String(), errb.String()
}

// gatService is a gat (huma) service with one operation, its schema
// endpoints under /api and a switchable /healthz.
type gatService struct {
	srv    *httptest.Server
	g      *gat.Gateway
	health atomic.Int32
}

type invoiceOut struct {
	Body struct {
		ID string `json:"id"`
	}
}

func newGatService(t *testing.T) *gatService {
	t.Helper()
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("billing", "1.0.0")
	cfg.DocsPath = ""
	cfg.CreateHooks = nil
	api := humago.New(mux, cfg)
	g, err := gat.New()
	if err != nil {
		t.Fatal(err)
	}
	gat.Register(api, g, huma.Operation{OperationID: "getInvoice", Method: http.MethodGet, Path: "/invoices/{id}"},
		func(ctx context.Context, in *struct {
			ID string `path:"id"`
		}) (*invoiceOut, error) {
			out := &invoiceOut{}
			out.Body.ID = in.ID
			return out, nil
		})
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		t.Fatal(err)
	}
	s := &gatService{g: g}
	s.health.Store(200)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(s.health.Load())) })
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// writeGatProfile writes the spec a registration client registers
// (OpenAPISpec("v1")) and a profile naming gat's schema endpoint.
func writeGatProfile(t *testing.T, s *gatService) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	spec, err := s.g.OpenAPISpec("v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "billing-v1.json"), spec, 0o644); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "gwag.yaml")
	code, out, errs := runSvc("init", "--spec", filepath.Join(dir, "billing-v1.json"), "--service", "billing",
		"--url", "/api/schema/openapi?version={version}", "--health", "/healthz",
		"--owner-team", "payments", "-o", path)
	if code != 0 {
		t.Fatalf("init: code=%d out=%q err=%q", code, out, errs)
	}
	return dir, path
}

func TestServiceInit(t *testing.T) {
	s := newGatService(t)
	_, path := writeGatProfile(t, s)
	p, err := profile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(profile.ModePush); err != nil {
		t.Fatal(err)
	}
	pv := p.Provides["v1"]
	if p.Service != "billing" || pv.Kind != "openapi" || pv.File != "billing-v1.json" ||
		pv.URL != "/api/schema/openapi?version=v1" || p.Health != "/healthz" || p.Owner.Team != "payments" {
		t.Fatalf("profile=%+v provides=%+v", p, pv)
	}
	// Refuses to overwrite; JSON output by extension.
	if code, _, _ := runSvc("init", "--spec", filepath.Join(filepath.Dir(path), "billing-v1.json"), "-o", path); code != 1 {
		t.Fatalf("overwrite: code=%d, want 1", code)
	}
	jsonPath := filepath.Join(filepath.Dir(path), "gwag.json")
	if code, _, errs := runSvc("init", "--spec", filepath.Join(filepath.Dir(path), "billing-v1.json"), "-o", jsonPath); code != 0 {
		t.Fatalf("json init: %s", errs)
	}
	b, _ := os.ReadFile(jsonPath)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m["service"] != "billing" {
		t.Fatalf("json profile=%s err=%v", b, err)
	}
	// Bad input: no spec.
	if code, _, _ := runSvc("init"); code != 2 {
		t.Fatalf("no spec: code=%d, want 2", code)
	}
	// Bad depends line.
	if code, _, _ := runSvc("init", "--spec", filepath.Join(filepath.Dir(path), "billing-v1.json"), "--depends", "library:x1", "-o", "-"); code != 3 {
		t.Fatalf("bad depends: code=%d, want 3", code)
	}
}

func TestServiceCheck_GatService(t *testing.T) {
	s := newGatService(t)
	dir, path := writeGatProfile(t, s)

	code, out, errs := runSvc("check", "--profile", path, "--url", s.srv.URL)
	if code != 0 {
		t.Fatalf("check: code=%d out=%s err=%s", code, out, errs)
	}
	if !strings.Contains(out, "service check: ok") || !strings.Contains(out, "warn") {
		t.Fatalf("out=%s, want ok and a warning for the unserved profile", out)
	}

	// --json
	code, out, _ = runSvc("check", "--profile", path, "--url", s.srv.URL, "--json")
	var res struct {
		OK       bool          `json:"ok"`
		ExitCode int           `json:"exitCode"`
		Checks   []checkResult `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || !res.OK || code != 0 || len(res.Checks) < 4 {
		t.Fatalf("json=%s err=%v", out, err)
	}

	// Health 500 → 4.
	s.health.Store(500)
	if code, out, _ := runSvc("check", "--profile", path, "--url", s.srv.URL); code != checkExitUnreachable {
		t.Fatalf("health 500: code=%d out=%s", code, out)
	}
	s.health.Store(200)

	// The local spec differs from the served one → 5.
	spec, _ := os.ReadFile(filepath.Join(dir, "billing-v1.json"))
	changed := strings.Replace(string(spec), `"getInvoice"`, `"getInvoiceRenamed"`, 1)
	if changed == string(spec) {
		t.Fatal("test spec has no getInvoice operationId")
	}
	if err := os.WriteFile(filepath.Join(dir, "billing-v1.json"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runSvc("check", "--profile", path, "--url", s.srv.URL)
	if code != checkExitSpec || !strings.Contains(out, "differs from the registered spec") {
		t.Fatalf("spec mismatch: code=%d out=%s", code, out)
	}

	// Unreachable service → 4.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if code, _, _ := runSvc("check", "--profile", path, "--url", dead.URL); code != checkExitUnreachable {
		t.Fatalf("unreachable: code=%d", code)
	}

	// Invalid profile → 3, before any network check.
	if err := os.WriteFile(path, []byte("service: billing\nprovides:\n  V1: {kind: soap}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runSvc("check", "--profile", path, "--url", dead.URL); code != checkExitProfile {
		t.Fatalf("bad profile: code=%d out=%s", code, out)
	}
}

func TestServiceCheck_WellKnownProfile(t *testing.T) {
	s := newGatService(t)
	_, path := writeGatProfile(t, s)
	p, _ := profile.Load(path)
	served, _ := p.JSON()
	mux := http.NewServeMux()
	mux.Handle("/", s.srv.Config.Handler)
	mux.HandleFunc(profile.WellKnownPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if code, out, _ := runSvc("check", "--profile", path, "--url", srv.URL, "--well-known"); code != 0 {
		t.Fatalf("well-known: code=%d out=%s", code, out)
	}
	// A served profile that says something else → 5.
	served = []byte(strings.Replace(string(served), `"payments"`, `"other"`, 1))
	if code, out, _ := runSvc("check", "--profile", path, "--url", srv.URL, "--well-known"); code != checkExitSpec {
		t.Fatalf("well-known mismatch: code=%d out=%s", code, out)
	}
	// --well-known on a service that serves none → 5.
	if code, _, _ := runSvc("check", "--profile", path, "--url", s.srv.URL, "--well-known"); code != checkExitSpec {
		t.Fatalf("well-known missing: code=%d", code)
	}
}

type greeterImpl struct {
	greeterv1.UnimplementedGreeterServiceServer
}

func (greeterImpl) Hello(_ context.Context, r *greeterv1.HelloRequest) (*greeterv1.HelloResponse, error) {
	return &greeterv1.HelloResponse{Greeting: "hello " + r.GetName()}, nil
}

// protoService is a gRPC greeter plus an HTTP side serving its
// .proto and /healthz.
func protoService(t *testing.T, src []byte) (grpcAddr string, httpSrv *httptest.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	greeterv1.RegisterGreeterServiceServer(gs, greeterImpl{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	mux := http.NewServeMux()
	mux.HandleFunc("/greeter.proto", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(src) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {})
	httpSrv = httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)
	return lis.Addr().String(), httpSrv
}

func TestServiceCheck_ProtoService(t *testing.T) {
	src, err := os.ReadFile("../../../examples/multi/protos/greeter.proto")
	if err != nil {
		t.Fatal(err)
	}
	grpcAddr, httpSrv := protoService(t, src)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "greeter.proto"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gwag.yaml")
	if code, _, errs := runSvc("init", "--spec", filepath.Join(dir, "greeter.proto"), "--url", "/greeter.proto", "--health", "/healthz", "-o", path); code != 0 {
		t.Fatalf("init: %s", errs)
	}
	p, _ := profile.Load(path)
	if p.Service != "greeter" || p.Provides["v1"].Kind != "proto" {
		t.Fatalf("profile=%+v", p)
	}
	code, out, _ := runSvc("check", "--profile", path, "--url", httpSrv.URL, "--grpc-addr", grpcAddr)
	if code != 0 || !strings.Contains(out, "grpc "+grpcAddr) {
		t.Fatalf("check: code=%d out=%s", code, out)
	}
	// gRPC down → 4.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	if code, out, _ := runSvc("check", "--profile", path, "--url", httpSrv.URL, "--grpc-addr", deadAddr, "--timeout", "1s"); code != checkExitUnreachable {
		t.Fatalf("grpc down: code=%d out=%s", code, out)
	}
}

// startControlPlane serves g's control plane on a local port.
func startControlPlane(t *testing.T, g *gateway.Gateway) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestServiceCheck_DependsOnGateway(t *testing.T) {
	s := newGatService(t)
	_, path := writeGatProfile(t, s)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, []byte("depends:\n  library: [v1, v2]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(g.Close)
	gw := startControlPlane(t, g)
	code, out, _ := runSvc("check", "--profile", path, "--gateway", gw)
	if code != checkExitDepends || !strings.Contains(out, "depends.library") {
		t.Fatalf("missing dep: code=%d out=%s", code, out)
	}
	lib, err := os.ReadFile("../../../examples/multi/protos/library.proto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr: "127.0.0.1:1", Services: []*cpv1.ServiceBinding{{Namespace: "library", Version: "v2", ProtoSource: lib}},
	}); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runSvc("check", "--profile", path, "--gateway", gw); code != 0 {
		t.Fatalf("dep present: code=%d out=%s", code, out)
	}
}

func TestServiceWait(t *testing.T) {
	s := newGatService(t)
	dir, path := writeGatProfile(t, s)
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure(),
		gateway.WithVerification(gateway.VerifyOptions{Interval: 100 * time.Millisecond, Retry: 50 * time.Millisecond}))
	t.Cleanup(g.Close)
	gw := startControlPlane(t, g)

	// Nothing registered yet → 4 after the timeout.
	if code, _, _ := runSvc("wait", "--gateway", gw, "--service", "billing:v1", "--green", "--timeout", "200ms", "--interval", "50ms"); code != waitExitNoneFound {
		t.Fatalf("none: code=%d", code)
	}

	// Register from the profile, as a service would.
	svcs, err := controlclient.LoadProfile(path, s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
		GatewayAddr: gw, ServiceAddr: s.srv.URL, Services: svcs, InstanceID: "billing-1",
		ReadyPollInterval: 50 * time.Millisecond, Logger: t.Logf,
	})
	if err != nil {
		t.Fatalf("SelfRegister: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	if bs := reg.Bindings(); len(bs) != 1 || bs[0].Verification != "green" {
		t.Fatalf("bindings=%+v, want green", bs)
	}

	code, out, errs := runSvc("wait", "--gateway", gw, "--service", "billing:v1", "--green", "--timeout", "5s", "--interval", "50ms")
	if code != waitExitGreen || !strings.Contains(out, "green billing/v1 billing-1") {
		t.Fatalf("green: code=%d out=%s err=%s", code, out, errs)
	}

	// Red: health fails; wait exits 3 with the reason.
	s.health.Store(500)
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
		if err == nil && len(matchInstances(resp, "billing", "v1", "")) == 1 &&
			matchInstances(resp, "billing", "v1", "")[0].Verification == "red" {
			break
		}
		if time.Now().After(deadline) {
			resp, _ := g.ControlPlane().ListServices(context.Background(), &cpv1.ListServicesRequest{})
			t.Fatalf("instance never went red: %v", resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, out, _ = runSvc("wait", "--gateway", gw, "--service", "billing", "--instance", "billing-1", "--timeout", "1s", "--interval", "50ms", "--json")
	if code != waitExitRed {
		t.Fatalf("red: code=%d out=%s", code, out)
	}
	var res struct {
		Verdict   string         `json:"verdict"`
		Instances []waitInstance `json:"instances"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Instances) != 1 ||
		res.Instances[0].Verification != "red" || len(res.Instances[0].Reasons) == 0 ||
		!strings.Contains(res.Instances[0].Reasons[0], "status 500") {
		t.Fatalf("red json=%s err=%v", out, err)
	}

	// Recovers.
	s.health.Store(200)
	if code, out, _ := runSvc("wait", "--gateway", gw, "--service", "billing:v1", "--timeout", "5s", "--interval", "50ms"); code != waitExitGreen {
		t.Fatalf("recovered: code=%d out=%s", code, out)
	}
	_ = dir
}

func TestServiceSchema(t *testing.T) {
	code, out, _ := runSvc("schema")
	var m map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &m) != nil || m["title"] != "gwag service profile" {
		t.Fatalf("schema: code=%d out=%.200s", code, out)
	}
}
