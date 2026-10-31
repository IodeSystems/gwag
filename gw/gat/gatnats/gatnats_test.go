package gatnats_test

import (
	"testing"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/gat/bustest"
	"github.com/iodesystems/gwag/v2/gw/gat/gatnats"
)

func startServer(t *testing.T) *natsd.Server {
	t.Helper()
	srv, err := natsd.NewServer(&natsd.Options{Host: "127.0.0.1", Port: -1, NoSigs: true, NoLog: true})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func connect(t *testing.T, srv *natsd.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestBus_Conformance(t *testing.T) {
	bustest.Run(t, func(t *testing.T) bustest.Harness {
		nc := connect(t, startServer(t))
		return bustest.Harness{Bus: gatnats.New(nc), Sync: func() { _ = nc.Flush() }}
	})
}

// TestBus_CrossConnection: two gat gateways on one NATS server see
// each other's publishes through the PubSub facade.
func TestBus_CrossConnection(t *testing.T) {
	srv := startServer(t)
	ncA, ncB := connect(t, srv), connect(t, srv)
	gA, _ := gat.New()
	gB, _ := gat.New()
	defer gA.Close()
	defer gB.Close()
	gA.UseBus(gatnats.New(ncA))
	gB.UseBus(gatnats.New(ncB))

	ch, cancel := gB.PubSub().Subscribe("orders.*")
	defer cancel()
	_ = ncB.Flush()
	gA.PubSub().Publish("orders.7", []byte("x"))
	select {
	case ev := <-ch:
		if ev.Channel != "orders.7" || string(ev.Payload) != "x" {
			t.Errorf("got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}
