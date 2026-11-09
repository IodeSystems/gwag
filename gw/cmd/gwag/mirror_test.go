package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

func mirrorFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// mirrorGateway starts a standalone cluster with opts and a gateway
// with admin token "tok" behind an httptest server.
func mirrorGateway(t *testing.T, opts gateway.ClusterOptions) (*gateway.Cluster, *gateway.Gateway, string) {
	t.Helper()
	opts.DataDir = t.TempDir()
	opts.ClientListen = mirrorFreeAddr(t)
	opts.ClusterListen = mirrorFreeAddr(t)
	opts.LogLevel = "silent"
	cl, err := gateway.StartCluster(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	g := gateway.New(gateway.WithCluster(cl), gateway.WithoutMetrics(), gateway.WithoutBackpressure(), gateway.WithAdminToken([]byte("tok")))
	t.Cleanup(g.Close)
	adminMux, _, err := g.AdminHumaRouter()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/admin/", http.StripPrefix("/api", g.AdminMiddleware(adminMux)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return cl, g, srv.URL
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestClusterMirrorCmd(t *testing.T) {
	mirrorAddr := mirrorFreeAddr(t)
	bcl, bgw, blue := mirrorGateway(t, gateway.ClusterOptions{NodeName: "blue", MirrorListen: mirrorAddr})
	src, err := os.ReadFile("../../../examples/events/proto/orders/v1/orders.proto")
	if err != nil {
		t.Fatal(err)
	}
	popts, err := os.ReadFile("../../proto/ps/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	if err := bgw.AddProtoBytes("orders/v1/orders.proto", src, gateway.To("127.0.0.1:1"), gateway.As("orders"),
		gateway.ProtoImports(map[string][]byte{"gw/proto/ps/v1/options.proto": popts})); err != nil {
		t.Fatal(err)
	}
	bgw.Handler()
	key := func(url string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runKey(append(args, "--endpoint", url, "--bearer", "tok"), &out, &errb)
		return code, out.String(), errb.String()
	}
	var sysKey string
	waitFor(t, "system key on blue", func() bool {
		code, out, _ := key(blue, "create", "--system", "--ttl", "1d")
		sysKey = strings.TrimSpace(out)
		return code == 0
	})
	const stream = "EV_orders_v1_OrderEvents"
	waitFor(t, "blue stream", func() bool {
		_, err := bcl.JS.Stream(context.Background(), stream)
		return err == nil
	})
	for _, id := range []string{"x1", "x2", "x3"} {
		m := nats.NewMsg("events.orders." + id + ".lifecycle")
		m.Header.Set("Nats-Msg-Id", id)
		if _, err := bcl.JS.PublishMsg(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}

	_, _, green := mirrorGateway(t, gateway.ClusterOptions{NodeName: "green", MirrorFrom: mirrorAddr, MirrorKey: sysKey})
	run := func(bearer string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runMirror(append(args, "--gateway", green, "--bearer", bearer), &out, &errb)
		return code, out.String(), errb.String()
	}
	row := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, stream+" ") {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		return ""
	}
	waitFor(t, "green sees blue's stream", func() bool {
		code, out, _ := run("tok", "--status")
		return code == 0 && row(out) == stream+" false 3 - - -"
	})
	if code, _, errs := run("tok", "--from", "elsewhere:1"); code != 1 || !strings.Contains(errs, "400") {
		t.Fatalf("--from elsewhere: %d %q", code, errs)
	}
	if code, _, errs := run("tok", "--status", "--stop"); code != 2 {
		t.Fatalf("--status --stop: %d %q", code, errs)
	}
	if code, out, errs := run("tok", "--from", mirrorAddr, "--channel", stream); code != 0 || !strings.HasPrefix(row(out), stream+" true 3") {
		t.Fatalf("start: %d %q %q", code, out, errs)
	}
	waitFor(t, "mirrored, lag 0", func() bool {
		code, out, _ := run("tok", "--status")
		return code == 0 && strings.HasPrefix(row(out), stream+" true 3 3 0 ")
	})

	// A system key of green works; an app key does not.
	var greenSys, greenApp string
	waitFor(t, "keys on green", func() bool {
		c1, o1, _ := key(green, "create", "--system", "--ttl", "1d")
		c2, o2, _ := key(green, "create", "--ns", "orders", "--ttl", "1d")
		greenSys, greenApp = strings.TrimSpace(o1), strings.TrimSpace(o2)
		return c1 == 0 && c2 == 0
	})
	if code, out, errs := run(greenSys, "--status"); code != 0 || row(out) == "" {
		t.Fatalf("status with a system key: %d %q %q", code, out, errs)
	}
	if code, _, errs := run(greenApp, "--stop"); code != 1 || !strings.Contains(errs, "401") {
		t.Fatalf("stop with an app key: %d %q", code, errs)
	}
	if code, out, errs := run(greenSys, "--stop"); code != 0 || !strings.HasPrefix(row(out), stream+" false 3 3 - -") {
		t.Fatalf("stop: %d %q %q", code, out, errs)
	}
}

func TestMirrorClusterOptions(t *testing.T) {
	t.Setenv("GWAG_MIRROR_KEY", "")
	var o gateway.ClusterOptions
	if err := mirrorClusterOptions(&o, "", "blue:1", "", "", "", ""); err == nil || !strings.Contains(err.Error(), "GWAG_MIRROR_KEY") {
		t.Fatalf("no key: %v", err)
	}
	f := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(f, []byte("gws_a_b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mirrorClusterOptions(&o, "", "blue:1", f, "", "", ""); err != nil || o.MirrorKey != "gws_a_b" || o.MirrorFrom != "blue:1" {
		t.Fatalf("key file: %v %+v", err, o)
	}
	t.Setenv("GWAG_MIRROR_KEY", "gws_c_d")
	o = gateway.ClusterOptions{}
	if err := mirrorClusterOptions(&o, "", "blue:1", "", "", "", ""); err != nil || o.MirrorKey != "gws_c_d" {
		t.Fatalf("env: %v %+v", err, o)
	}
	if err := mirrorClusterOptions(&o, "", "", f, "", "", ""); err == nil {
		t.Fatal("key file without --mirror-from: accepted")
	}
	if err := mirrorClusterOptions(&o, "", "", "", "c", "k", "ca"); err == nil {
		t.Fatal("tls without listen or from: accepted")
	}
}
