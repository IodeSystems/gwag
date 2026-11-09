package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

const (
	vGreen   = cpv1.VerificationState_VERIFICATION_STATE_GREEN
	vRed     = cpv1.VerificationState_VERIFICATION_STATE_RED
	vNone    = cpv1.VerificationState_VERIFICATION_STATE_UNSPECIFIED
	fastPoll = 50 * time.Millisecond
)

// probeSide is the HTTP side of a fake instance: /healthz and /spec,
// both switchable.
type probeSide struct {
	srv    *httptest.Server
	health atomic.Int32
	mu     sync.Mutex
	spec   []byte
}

func newProbeSide(t *testing.T, spec []byte) *probeSide {
	t.Helper()
	p := &probeSide{spec: spec}
	p.health.Store(200)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(int(p.health.Load()))
		case "/spec":
			p.mu.Lock()
			b := p.spec
			p.mu.Unlock()
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *probeSide) setSpec(b []byte) {
	p.mu.Lock()
	p.spec = b
	p.mu.Unlock()
}

func (p *probeSide) verify() *cpv1.Verify {
	return &cpv1.Verify{HealthUrl: p.srv.URL + "/healthz", SpecUrl: p.srv.URL + "/spec"}
}

func verifiedGreeter(t *testing.T, p *probeSide) *cpv1.ServiceBinding {
	b := greeterBinding(t, "v1")
	b.Verify = p.verify()
	return b
}

func newVerifyGateway(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	g := New(WithoutMetrics(), WithoutBackpressure(), WithVerification(VerifyOptions{
		Interval: 100 * time.Millisecond,
		Retry:    50 * time.Millisecond,
		Timeout:  2 * time.Second,
	}))
	t.Cleanup(g.Close)
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(srv.Close)
	return g, srv
}

func instanceOf(t *testing.T, g *Gateway, regID string) *cpv1.InstanceStatus {
	t.Helper()
	_, info := listServiceInfo(t, g, "greeter", "v1")
	if info == nil {
		return nil
	}
	for _, in := range info.GetInstances() {
		if in.GetRegistrationId() == regID {
			return in
		}
	}
	return nil
}

func TestVerify_GreenRoutable(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	p := newProbeSide(t, testProtoBytes(t, "greeter.proto"))
	g, srv := newVerifyGateway(t)

	resp := mustRegister(t, g, addr, verifiedGreeter(t, p))
	wantStates(t, resp.GetBindings(), stReady)
	if v := resp.GetBindings()[0].GetVerification(); v != vGreen {
		t.Fatalf("verification=%v reasons=%v, want GREEN", v, resp.GetBindings()[0].GetVerifyReasons())
	}
	if got, body := helloVia(t, srv.URL); got != "hello x" {
		t.Fatalf("hello: %s", body)
	}
	if greeter.helloCalls.Load() != 1 {
		t.Fatalf("helloCalls=%d", greeter.helloCalls.Load())
	}
	in := instanceOf(t, g, resp.GetRegistrationId())
	if in == nil || in.GetVerification() != vGreen || in.GetVerifiedUnixMs() == 0 {
		t.Fatalf("listing=%v, want green with a probe time", in)
	}
}

func TestVerify_SpecMismatchRedNotRoutableThenGreen(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	good := testProtoBytes(t, "greeter.proto")
	p := newProbeSide(t, []byte(strings.Replace(string(good), "message HelloRequest {", "message HelloRequest {\n  string extra = 99;", 1)))
	g, srv := newVerifyGateway(t)

	resp := mustRegister(t, g, addr, verifiedGreeter(t, p))
	wantStates(t, resp.GetBindings(), stWaiting)
	b := resp.GetBindings()[0]
	if b.GetVerification() != vRed || len(b.GetVerifyReasons()) != 1 || !strings.Contains(b.GetVerifyReasons()[0], "differs from the registered spec") {
		t.Fatalf("verification=%v reasons=%v, want RED with a spec reason", b.GetVerification(), b.GetVerifyReasons())
	}
	if got, _ := helloVia(t, srv.URL); got != "" {
		t.Fatalf("red greeter answered %q", got)
	}
	if n := greeter.helloCalls.Load(); n != 0 {
		t.Fatalf("helloCalls=%d while red", n)
	}
	if in := instanceOf(t, g, resp.GetRegistrationId()); in == nil || in.GetState() != stWaiting || in.GetVerification() != vRed {
		t.Fatalf("listing=%v, want waiting + red", in)
	}

	p.setSpec(good)
	waitGate(t, 5*time.Second, "green after the spec is fixed", func() bool {
		st := heartbeatStates(t, g, resp.GetRegistrationId())
		return st[0].GetState() == stReady && st[0].GetVerification() == vGreen
	})
	if got, body := helloVia(t, srv.URL); got != "hello x" {
		t.Fatalf("hello after green: %s", body)
	}
}

func TestVerify_HealthFailsRedOtherInstanceServesThenRecovers(t *testing.T) {
	g1, addr1 := startGreeterServer(t)
	g2, addr2 := startGreeterServer(t)
	spec := testProtoBytes(t, "greeter.proto")
	p1, p2 := newProbeSide(t, spec), newProbeSide(t, spec)
	g, srv := newVerifyGateway(t)

	r1 := mustRegister(t, g, addr1, verifiedGreeter(t, p1))
	r2 := mustRegister(t, g, addr2, verifiedGreeter(t, p2))
	wantStates(t, r1.GetBindings(), stReady)
	wantStates(t, r2.GetBindings(), stReady)

	p1.health.Store(500)
	waitGate(t, 5*time.Second, "instance 1 red", func() bool {
		in := instanceOf(t, g, r1.GetRegistrationId())
		return in != nil && in.GetVerification() == vRed && in.GetState() == stWaiting
	})
	in := instanceOf(t, g, r1.GetRegistrationId())
	if len(in.GetVerifyReasons()) != 1 || !strings.Contains(in.GetVerifyReasons()[0], "status 500") {
		t.Fatalf("reasons=%v, want the health status", in.GetVerifyReasons())
	}
	before1 := g1.helloCalls.Load()
	for i := 0; i < 10; i++ {
		if got, body := helloVia(t, srv.URL); got != "hello x" {
			t.Fatalf("hello with one red instance: %s", body)
		}
	}
	if g1.helloCalls.Load() != before1 {
		t.Fatalf("red instance got %d calls", g1.helloCalls.Load()-before1)
	}
	if g2.helloCalls.Load() < 10 {
		t.Fatalf("green instance got %d calls, want 10", g2.helloCalls.Load())
	}
	// The red instance is not ready for the start gate either.
	g.mu.Lock()
	ready := g.readyInstancesLocked("greeter", "v1")
	g.mu.Unlock()
	if ready != 1 {
		t.Fatalf("ready instances=%d, want 1", ready)
	}

	p1.health.Store(200)
	waitGate(t, 5*time.Second, "instance 1 green again", func() bool {
		in := instanceOf(t, g, r1.GetRegistrationId())
		return in != nil && in.GetVerification() == vGreen && in.GetState() == stReady
	})
	g.mu.Lock()
	ready = g.readyInstancesLocked("greeter", "v1")
	g.mu.Unlock()
	if ready != 2 {
		t.Fatalf("ready instances=%d after recovery, want 2", ready)
	}
}

func TestVerify_LastInstanceRedNoTraffic(t *testing.T) {
	greeter, addr := startGreeterServer(t)
	p := newProbeSide(t, testProtoBytes(t, "greeter.proto"))
	g, srv := newVerifyGateway(t)
	resp := mustRegister(t, g, addr, verifiedGreeter(t, p))
	wantStates(t, resp.GetBindings(), stReady)

	p.health.Store(503)
	waitGate(t, 5*time.Second, "red", func() bool {
		st := heartbeatStates(t, g, resp.GetRegistrationId())
		return st[0].GetVerification() == vRed && st[0].GetState() == stWaiting
	})
	before := greeter.helloCalls.Load()
	if got, _ := helloVia(t, srv.URL); got != "" {
		t.Fatalf("red last instance answered %q", got)
	}
	if greeter.helloCalls.Load() != before {
		t.Fatal("red last instance got a call")
	}
}

func TestVerify_UnreachableAddressRed(t *testing.T) {
	_, addr := startGreeterServer(t)
	p := newProbeSide(t, testProtoBytes(t, "greeter.proto"))
	g, _ := newVerifyGateway(t)
	b := verifiedGreeter(t, p)
	// A closed port on the same host.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadAddr := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	resp := mustRegister(t, g, deadAddr, b)
	wantStates(t, resp.GetBindings(), stWaiting)
	if r := resp.GetBindings()[0].GetVerifyReasons(); len(r) != 1 || !strings.Contains(r[0], "not reachable") {
		t.Fatalf("reasons=%v, want unreachable", r)
	}
	// The same binding on a live address is green.
	resp = mustRegister(t, g, addr, verifiedGreeter(t, p))
	wantStates(t, resp.GetBindings(), stReady)
}

func TestVerify_OtherHostRefused(t *testing.T) {
	_, addr := startGreeterServer(t)
	g, _ := newVerifyGateway(t)
	b := greeterBinding(t, "v1")
	b.Verify = &cpv1.Verify{HealthUrl: "http://169.254.169.254/latest/meta-data"}
	_, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{b}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "is not the service's host") {
		t.Fatalf("err=%v, want InvalidArgument for another host", err)
	}
}

func TestVerify_NotDeclaredUnchanged(t *testing.T) {
	_, addr := startGreeterServer(t)
	g, _ := newVerifyGateway(t)
	resp := mustRegister(t, g, addr, greeterBinding(t, "v1"))
	wantStates(t, resp.GetBindings(), stReady)
	if v := resp.GetBindings()[0].GetVerification(); v != vNone {
		t.Fatalf("verification=%v, want UNSPECIFIED", v)
	}
}

func TestVerify_DependencyMissingStillWaiting(t *testing.T) {
	_, addr := startGreeterServer(t)
	p := newProbeSide(t, testProtoBytes(t, "greeter.proto"))
	g, _ := newVerifyGateway(t)
	resp := mustRegister(t, g, addr, dependsOn(verifiedGreeter(t, p), "library", "v1"))
	wantStates(t, resp.GetBindings(), stWaiting)
	b := resp.GetBindings()[0]
	if b.GetVerification() != vGreen {
		t.Fatalf("verification=%v, want GREEN (only the dependency is missing)", b.GetVerification())
	}
	if d := b.GetDepends(); len(d) != 1 || len(d[0].GetPresentLines()) != 0 {
		t.Fatalf("depends=%v, want library missing", d)
	}
	mustRegister(t, g, addr, libraryBinding(t, "v1"))
	waitGate(t, 5*time.Second, "ready once library is up", func() bool {
		return heartbeatStates(t, g, resp.GetRegistrationId())[0].GetState() == stReady
	})
}

func TestVerify_ClusterRedReachesEveryNode(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	for _, n := range []*clusterNode{a, b} {
		n.gw.cfg.verify = VerifyOptions{Interval: 100 * time.Millisecond, Retry: 50 * time.Millisecond, Timeout: 2 * time.Second}
	}
	greeter, addr := startGreeterServer(t)
	p := newProbeSide(t, testProtoBytes(t, "greeter.proto"))

	var resp *cpv1.RegisterResponse
	waitGate(t, 30*time.Second, "register on A", func() bool {
		r, err := a.gw.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{Addr: addr, Services: []*cpv1.ServiceBinding{verifiedGreeter(t, p)}})
		resp = r
		return err == nil
	})
	wantStates(t, resp.GetBindings(), stReady)
	waitForPool(t, b.gw, "greeter", "v1", 30*time.Second)
	if got, body := helloVia(t, b.httpSrv.URL); got != "hello x" {
		t.Fatalf("hello via B: %s", body)
	}

	p.health.Store(500)
	waitGate(t, 15*time.Second, "B lists the instance red", func() bool {
		_, info := listServiceInfo(t, b.gw, "greeter", "v1")
		return info != nil && len(info.GetInstances()) == 1 &&
			info.GetInstances()[0].GetVerification() == vRed && info.GetInstances()[0].GetState() == stWaiting
	})
	waitGate(t, 15*time.Second, "B stops routing", func() bool {
		b.gw.mu.Lock()
		defer b.gw.mu.Unlock()
		return b.gw.readyInstancesLocked("greeter", "v1") == 0
	})
	before := greeter.helloCalls.Load()
	if got, _ := helloVia(t, b.httpSrv.URL); got != "" {
		t.Fatalf("red instance routable via B: %q", got)
	}
	if greeter.helloCalls.Load() != before {
		t.Fatal("red instance got a call via B")
	}

	p.health.Store(200)
	waitGate(t, 15*time.Second, "B routes again", func() bool {
		got, _ := helloVia(t, b.httpSrv.URL)
		return got == "hello x"
	})
}
