package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// openAPIService is an HTTP service with no registration client: it
// serves its profile, spec, health and one operation.
func openAPIService(t *testing.T, health *atomic.Int32) *httptest.Server {
	t.Helper()
	spec := `{"openapi":"3.0.3","info":{"title":"billing","version":"1"},
"paths":{"/invoices/{id}":{"get":{"operationId":"getInvoice",
"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"id":{"type":"string"}}}}}}}}}}}`
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/gwag.json":
			_, _ = io.WriteString(w, `{"service":"billing","owner":{"team":"payments"},"health":"/healthz",
"provides":{"v1":{"kind":"openapi","url":"/openapi.json"}}}`)
		case r.URL.Path == "/openapi.json":
			_, _ = io.WriteString(w, spec)
		case r.URL.Path == "/healthz":
			w.WriteHeader(int(health.Load()))
		case strings.HasPrefix(r.URL.Path, "/invoices/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": strings.TrimPrefix(r.URL.Path, "/invoices/")})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPulledProfile_Green(t *testing.T) {
	var health atomic.Int32
	health.Store(200)
	svc := openAPIService(t, &health)
	g, srv := newVerifyGateway(t)
	cp := g.ControlPlane()
	profileURL := svc.URL + "/.well-known/gwag.json"

	resp, err := cp.AddServiceProfile(context.Background(), &cpv1.AddServiceProfileRequest{Url: profileURL})
	if err != nil {
		t.Fatalf("AddServiceProfile: %v", err)
	}
	if resp.GetService() != "billing" || len(resp.GetBindings()) != 1 {
		t.Fatalf("resp=%v", resp)
	}
	if b := resp.GetBindings()[0]; b.GetState() != stReady || b.GetVerification() != vGreen {
		t.Fatalf("binding=%v, want READY + GREEN", b)
	}
	_, info := listServiceInfo(t, g, "billing", "v1")
	if info == nil || len(info.GetInstances()) != 1 || info.GetInstances()[0].GetInstanceId() != "pull:"+profileURL ||
		info.GetOwner().GetTeam() != "payments" {
		t.Fatalf("listing=%v", info)
	}
	// Routable through the gateway.
	q := `{"query":"{ billing { getInvoice(id:\"7\") { id } } }"}`
	r, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), `"id":"7"`) {
		t.Fatalf("getInvoice: %s", body)
	}

	list, err := cp.ListServiceProfiles(context.Background(), &cpv1.ListServiceProfilesRequest{})
	if err != nil || len(list.GetProfiles()) != 1 || list.GetProfiles()[0].GetService() != "billing" {
		t.Fatalf("profiles=%v err=%v", list, err)
	}

	// The gateway holds the registration: it stays past the TTL with
	// no heartbeat from the service. (Heartbeats are internal; check
	// the registration still answers.)
	regID := info.GetInstances()[0].GetRegistrationId()
	if !heartbeatOK(g, regID) {
		t.Fatal("pulled registration not live")
	}

	// A failing health check turns it red, like a pushed service.
	health.Store(500)
	waitGate(t, 5*time.Second, "pulled service red", func() bool {
		_, info := listServiceInfo(t, g, "billing", "v1")
		return info != nil && len(info.GetInstances()) == 1 && info.GetInstances()[0].GetVerification() == vRed
	})

	rm, err := cp.RemoveServiceProfile(context.Background(), &cpv1.RemoveServiceProfileRequest{Url: profileURL})
	if err != nil || !rm.GetRemoved() {
		t.Fatalf("remove=%v err=%v", rm, err)
	}
	if _, info := listServiceInfo(t, g, "billing", "v1"); info != nil {
		t.Fatalf("billing still listed after remove: %v", info)
	}
}

func heartbeatOK(g *Gateway, regID string) bool {
	resp, err := g.ControlPlane().Heartbeat(context.Background(), &cpv1.HeartbeatRequest{RegistrationId: regID})
	return err == nil && resp.GetOk()
}

func TestPulledProfile_Refusals(t *testing.T) {
	var health atomic.Int32
	health.Store(200)
	svc := openAPIService(t, &health)
	g := New(WithoutMetrics(), WithoutBackpressure(), WithVerification(VerifyOptions{ProfileHosts: []string{"*.internal"}}))
	t.Cleanup(g.Close)
	_, err := g.ControlPlane().AddServiceProfile(context.Background(), &cpv1.AddServiceProfileRequest{Url: svc.URL + "/.well-known/gwag.json"})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "not in the allowed profile hosts") {
		t.Fatalf("err=%v, want refused by the allow-list", err)
	}

	g2, _ := newVerifyGateway(t)
	_, err = g2.ControlPlane().AddServiceProfile(context.Background(), &cpv1.AddServiceProfileRequest{Url: svc.URL + "/missing"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err=%v, want InvalidArgument for a missing profile", err)
	}
	list, _ := g2.ControlPlane().ListServiceProfiles(context.Background(), &cpv1.ListServiceProfilesRequest{})
	if len(list.GetProfiles()) != 0 {
		t.Fatalf("a refused profile was stored: %v", list)
	}
}

func TestPulledProfile_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	a := newSharedClusterNode(t, testCluster.a)
	b := newSharedClusterNode(t, testCluster.b)
	waitForPeers(t, a.gw, b.gw)
	var health atomic.Int32
	health.Store(200)
	svc := openAPIService(t, &health)
	profileURL := svc.URL + "/.well-known/gwag.json"

	waitGate(t, 30*time.Second, "add on B", func() bool {
		_, err := b.gw.ControlPlane().AddServiceProfile(context.Background(), &cpv1.AddServiceProfileRequest{Url: profileURL})
		return err == nil
	})
	// Exactly one node holds the registration; both list it green.
	for _, n := range []*clusterNode{a, b} {
		waitGate(t, 30*time.Second, "billing green on "+n.gw.nodeID(), func() bool {
			_, info := listServiceInfo(t, n.gw, "billing", "v1")
			return info != nil && len(info.GetInstances()) == 1 &&
				info.GetInstances()[0].GetVerification() == vGreen && info.GetInstances()[0].GetState() == stReady
		})
	}
	held := 0
	for _, n := range []*clusterNode{a, b} {
		n.gw.pulled.mu.Lock()
		for _, l := range n.gw.pulled.local {
			if l.regID != "" {
				held++
			}
		}
		n.gw.pulled.mu.Unlock()
	}
	if held != 1 {
		t.Fatalf("%d nodes hold the pulled registration, want 1", held)
	}
	list, err := a.gw.ControlPlane().ListServiceProfiles(context.Background(), &cpv1.ListServiceProfilesRequest{})
	if err != nil || len(list.GetProfiles()) != 1 {
		t.Fatalf("profiles on A=%v err=%v", list, err)
	}
	rm, err := a.gw.ControlPlane().RemoveServiceProfile(context.Background(), &cpv1.RemoveServiceProfileRequest{Url: profileURL})
	if err != nil || !rm.GetRemoved() {
		t.Fatalf("remove=%v err=%v", rm, err)
	}
	for _, n := range []*clusterNode{a, b} {
		waitGate(t, 30*time.Second, "billing gone on "+n.gw.nodeID(), func() bool {
			_, info := listServiceInfo(t, n.gw, "billing", "v1")
			return info == nil
		})
	}
}
