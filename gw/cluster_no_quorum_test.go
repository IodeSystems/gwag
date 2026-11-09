package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// TestClusterNoQuorum_SurvivorKeepsRouting: 2 of 3 nodes die, so
// JetStream has no quorum. The survivor keeps routing every service
// in the registry it holds; only writes fail.
//
//   - greeter registered through the survivor. Its heartbeats keep
//     answering ok before the client's deadline, so the client stays
//     and the survivor's janitor does not evict it. Before the fix the
//     registry refresh waited out the client's deadline, the client
//     moved to a dead address, and the survivor dropped greeter one
//     TTL later while the instance was still up.
//   - library registered through a node that died, and knows no other
//     address. No delete reaches the survivor, so it keeps the route.
//   - health stays 200 "serving" and reports "jetstream":"unavailable".
//   - a new registration fails.
func TestClusterNoQuorum_SurvivorKeepsRouting(t *testing.T) {
	nodes := startExpiryCluster(t)
	survivor, dead := nodes[0], nodes[1:]
	greeter := selfRegister(t, "greeter", survivor.cpAddr, dead[0].cpAddr, dead[1].cpAddr)
	library := selfRegister(t, "library", dead[0].cpAddr)
	waitUntilErr(t, 15*time.Second, "the survivor routes greeter and library", func() error {
		for _, ns := range []string{"greeter", "library"} {
			if o := owners(survivor.gw, ns); len(o) != 1 {
				return fmt.Errorf("%s owners %v", ns, o)
			}
		}
		return nil
	})
	greeterID, libraryID := greeter.ID(), library.ID()
	if code, body := probeHealth(t, survivor.gw); code != http.StatusOK || body["jetstream"] != "ok" || body["degraded"] != nil {
		t.Fatalf("health before the kill: %d %v, want 200 with jetstream ok and no degraded", code, body)
	}

	conn, err := grpc.NewClient(survivor.cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cp := cpv1.NewControlPlaneClient(conn)

	for _, n := range dead {
		n.kill()
	}
	killedAt := time.Now()

	// A service keeps trying to register on the survivor, as a client
	// that fails over does. Each Register waits out its KV put; before
	// the fix it held the control plane's lock meanwhile, and every
	// heartbeat to the node timed out behind it.
	stopReg := make(chan struct{})
	regDone := make(chan []error, 1)
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopReg) }) }
	defer stop()
	lateProto := testProtoBytes(t, "greeter.proto")
	go func() {
		var errs []error
		for {
			select {
			case <-stopReg:
				regDone <- errs
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err := cp.Register(ctx, &cpv1.RegisterRequest{
				Addr:       "127.0.0.1:2",
				InstanceId: "late",
				Services:   []*cpv1.ServiceBinding{{Namespace: "late", Version: "v1", ProtoSource: lateProto}},
			})
			cancel()
			errs = append(errs, err)
		}
	}()

	// Without the fix the janitor evicted greeter one TTL after the
	// last heartbeat it saw (seen at 20.0s); give it a janitor period
	// and more.
	end := killedAt.Add(expiryTTL + 2*janitorPeriod)
	var unavailableAt time.Duration
	for time.Now().Before(end) {
		if o := owners(survivor.gw, "greeter"); len(o) != 1 || o[0] != greeterID {
			t.Fatalf("%s after the kill: survivor routes greeter to %v, want [%s]", time.Since(killedAt).Round(time.Millisecond), o, greeterID)
		}
		if o := owners(survivor.gw, "library"); len(o) != 1 || o[0] != libraryID {
			t.Fatalf("%s after the kill: survivor routes library to %v, want [%s]", time.Since(killedAt).Round(time.Millisecond), o, libraryID)
		}
		code, body := probeHealth(t, survivor.gw)
		if code != http.StatusOK || body["status"] != "serving" {
			t.Fatalf("%s after the kill: health %d %v, want 200 serving", time.Since(killedAt).Round(time.Millisecond), code, body)
		}
		if unavailableAt == 0 && body["jetstream"] == "unavailable" {
			unavailableAt = time.Since(killedAt)
			if body["degraded"] != true {
				t.Errorf("health %v: jetstream unavailable without degraded", body)
			}
			t.Logf("health reports jetstream unavailable %s after the kill", unavailableAt.Round(time.Millisecond))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if unavailableAt == 0 {
		_, body := probeHealth(t, survivor.gw)
		t.Errorf("health never reported jetstream unavailable: %v", body)
	}
	if greeter.GatewayAddr() != survivor.cpAddr {
		t.Errorf("greeter's client moved to %s, want it to stay on the survivor %s", greeter.GatewayAddr(), survivor.cpAddr)
	}

	// Writes fail: no registration was stored.
	stop()
	errs := <-regDone
	if len(errs) == 0 {
		t.Fatalf("no Register call finished")
	}
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), "kv put") {
			t.Fatalf("Register %d without a JetStream quorum: %v, want a kv put error", i, err)
		}
	}
	t.Logf("%d Register calls without quorum, all refused: %v", len(errs), errs[0])
}
