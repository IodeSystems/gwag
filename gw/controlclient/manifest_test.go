package controlclient

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// captureCP records the RegisterRequest it receives.
type captureCP struct {
	cpv1.UnimplementedControlPlaneServer
	mu  sync.Mutex
	req *cpv1.RegisterRequest
}

func (c *captureCP) Register(_ context.Context, req *cpv1.RegisterRequest) (*cpv1.RegisterResponse, error) {
	c.mu.Lock()
	c.req = req
	c.mu.Unlock()
	return &cpv1.RegisterResponse{RegistrationId: "r1", TtlSeconds: 30}, nil
}

func (c *captureCP) Deregister(context.Context, *cpv1.DeregisterRequest) (*cpv1.DeregisterResponse, error) {
	return &cpv1.DeregisterResponse{}, nil
}

func TestSelfRegister_SendsManifest(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cp := &captureCP{}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, cp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	proto := []byte(`syntax = "proto3"; package orders.v1; message P { string n = 1; } service S { rpc M(P) returns (P); }`)
	reg, err := SelfRegister(context.Background(), Options{
		GatewayAddr: lis.Addr().String(),
		ServiceAddr: "orders:50051",
		Services: []Service{
			{
				Namespace:   "orders",
				Version:     "v1",
				ProtoSource: proto,
				Depends:     []Dependency{{Service: "library", Lines: []string{"v1", "v2"}}},
				Owner:       &Owner{Team: "orders", Contact: "#orders"},
				Public:      true,
			},
			{Namespace: "plain", OpenAPISpec: []byte(`{}`)},
		},
	})
	if err != nil {
		t.Fatalf("SelfRegister: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	cp.mu.Lock()
	req := cp.req
	cp.mu.Unlock()
	if req == nil || len(req.GetServices()) != 2 {
		t.Fatalf("captured request=%v", req)
	}
	b := req.GetServices()[0]
	if d := b.GetDepends(); len(d) != 1 || d[0].GetService() != "library" ||
		len(d[0].GetLines()) != 2 || d[0].GetLines()[0] != "v1" || d[0].GetLines()[1] != "v2" {
		t.Errorf("depends=%v", d)
	}
	if o := b.GetOwner(); o.GetTeam() != "orders" || o.GetContact() != "#orders" {
		t.Errorf("owner=%v", o)
	}
	if !b.GetPublic() {
		t.Error("public=false, want true")
	}
	plain := req.GetServices()[1]
	if len(plain.GetDepends()) != 0 || plain.GetOwner() != nil || plain.GetPublic() {
		t.Errorf("binding without manifest sent depends=%v owner=%v", plain.GetDepends(), plain.GetOwner())
	}
}
