package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"

	gateway "github.com/iodesystems/gwag/v2/gw"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

func TestDrainCmd(t *testing.T) {
	src, err := os.ReadFile("../../../examples/multi/protos/greeter.proto")
	if err != nil {
		t.Fatal(err)
	}
	g := gateway.New(gateway.WithoutMetrics(), gateway.WithoutBackpressure())
	t.Cleanup(g.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cpv1.RegisterControlPlaneServer(srv, g.ControlPlane())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	gw := lis.Addr().String()

	resp, err := g.ControlPlane().Register(context.Background(), &cpv1.RegisterRequest{
		Addr:       "127.0.0.1:1",
		InstanceId: "greeter-pod-1",
		Services:   []*cpv1.ServiceBinding{{Namespace: "greeter", Version: "v1", ProtoSource: src}},
	})
	if err != nil {
		t.Fatal(err)
	}
	regID := resp.GetRegistrationId()

	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runDrain(args, &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, out, _ := run("--list", "--gateway", gw); code != 0 || !strings.Contains(out, regID) || !strings.Contains(out, "greeter-pod-1") {
		t.Fatalf("--list: code=%d out=%q", code, out)
	}
	code, out, errs := run("--wait", "greeter-pod-1", "--gateway", gw, "--interval", "50ms", "--timeout", "10s")
	if code != 0 || !strings.Contains(out, "safe_to_stop") || !strings.Contains(out, regID) {
		t.Fatalf("--wait: code=%d out=%q err=%q", code, out, errs)
	}
	if code, out, _ := run("--cancel", regID, "--gateway", gw); code != 0 || !strings.Contains(out, "serving") {
		t.Fatalf("--cancel: code=%d out=%q", code, out)
	}
	if code, _, errs := run("nope", "--gateway", gw); code != 1 || !strings.Contains(errs, "NotFound") {
		t.Fatalf("unknown: code=%d err=%q", code, errs)
	}
	if code, _, _ := run("--gateway", gw); code != 2 {
		t.Fatalf("no instance: code=%d, want 2", code)
	}
}
