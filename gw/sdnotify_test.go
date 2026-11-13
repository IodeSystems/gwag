package gateway

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func fakeNotifySocket(t *testing.T) (string, *net.UnixConn) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "notify.sock")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: p, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return p, c
}

func readDatagram(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no datagram: %v", err)
	}
	return string(buf[:n])
}

func TestSdNotifyUnsetIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
}

func TestSdNotifyWritesDatagram(t *testing.T) {
	p, c := fakeNotifySocket(t)
	t.Setenv("NOTIFY_SOCKET", p)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
	if got := readDatagram(t, c); got != "READY=1" {
		t.Errorf("got %q", got)
	}
}

func TestNotifyWhenReadyWaitsForServing(t *testing.T) {
	p, c := fakeNotifySocket(t)
	t.Setenv("NOTIFY_SOCKET", p)
	g := &Gateway{}
	g.registrySync.Store(registrySyncPending)
	stop := make(chan struct{})
	defer close(stop)
	go notifyWhenReady(g, stop)

	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := c.Read(make([]byte, 16)); err == nil {
		t.Fatalf("READY sent while starting (%d bytes)", n)
	}
	g.registrySync.Store(registrySyncDone)
	if got := readDatagram(t, c); got != "READY=1" {
		t.Errorf("got %q", got)
	}
}

func TestNotifyWhenReadyStops(t *testing.T) {
	p, c := fakeNotifySocket(t)
	t.Setenv("NOTIFY_SOCKET", p)
	g := &Gateway{}
	g.registrySync.Store(registrySyncPending)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { notifyWhenReady(g, stop); close(done) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop")
	}
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := c.Read(make([]byte, 16)); err == nil {
		t.Error("datagram after stop")
	}
}
