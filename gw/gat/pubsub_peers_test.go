package gat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var meshKey = []byte("shared-mesh-secret")

// TestPeerMesh_CrossNodeDelivery: gat A publishes, gat B's subscriber
// receives via the mesh.
func TestPeerMesh_CrossNodeDelivery(t *testing.T) {
	gB, _ := New()
	gB.EnablePeerMesh(PeerMesh{Auth: meshKey})
	defer gB.Close()
	srvB := httptest.NewServer(gB.peerPublishHandler())
	defer srvB.Close()
	ch, cancel := gB.PubSub().Subscribe("users.>")
	defer cancel()

	gA, _ := New()
	gA.EnablePeerMesh(PeerMesh{Peers: []string{srvB.URL}, Auth: meshKey})
	defer gA.Close()

	gA.PubSub().Publish("users.42", []byte("hello"))

	ev := recvWithin(t, ch)
	if ev.Channel != "users.42" || string(ev.Payload) != "hello" {
		t.Errorf("B received %+v; want users.42/hello", ev)
	}
}

// TestPeerMesh_NoRebroadcast: A and B peer each other. A publishes
// once; each side's subscriber must receive exactly one event — a
// received event fans out local-only, never back to the mesh.
func TestPeerMesh_NoRebroadcast(t *testing.T) {
	gA, _ := New()
	gB, _ := New()
	defer gA.Close()
	defer gB.Close()

	// Stand up both receive endpoints first, then peer them.
	muxA := http.NewServeMux()
	muxB := http.NewServeMux()
	srvA := httptest.NewServer(muxA)
	srvB := httptest.NewServer(muxB)
	defer srvA.Close()
	defer srvB.Close()

	gA.EnablePeerMesh(PeerMesh{Peers: []string{srvB.URL}, Auth: meshKey})
	gB.EnablePeerMesh(PeerMesh{Peers: []string{srvA.URL}, Auth: meshKey})
	muxA.Handle(PeerPublishPath, gA.peerPublishHandler())
	muxB.Handle(PeerPublishPath, gB.peerPublishHandler())

	chA, cancelA := gA.PubSub().Subscribe("e.>")
	chB, cancelB := gB.PubSub().Subscribe("e.>")
	defer cancelA()
	defer cancelB()

	gA.PubSub().Publish("e.1", []byte("x"))

	// A's subscriber: one local delivery.
	if ev := recvWithin(t, chA); string(ev.Payload) != "x" {
		t.Errorf("A subscriber got %+v", ev)
	}
	// B's subscriber: one mesh delivery.
	if ev := recvWithin(t, chB); string(ev.Payload) != "x" {
		t.Errorf("B subscriber got %+v", ev)
	}
	// Neither side should see a second copy — a rebroadcast from B
	// back to A would surface here.
	if _, ok := tryRecvAfter(chA, 200*time.Millisecond); ok {
		t.Error("A received a duplicate — event was rebroadcast")
	}
	if _, ok := tryRecvAfter(chB, 200*time.Millisecond); ok {
		t.Error("B received a duplicate — event was rebroadcast")
	}
}

// TestPeerMesh_BadSignatureRejected: a POST signed with the wrong key
// is rejected 401 and never fans out.
func TestPeerMesh_BadSignatureRejected(t *testing.T) {
	g, _ := New()
	g.EnablePeerMesh(PeerMesh{Auth: meshKey})
	defer g.Close()
	srv := httptest.NewServer(g.peerPublishHandler())
	defer srv.Close()
	ch, cancel := g.PubSub().Subscribe("c")
	defer cancel()

	body, _ := json.Marshal(peerEnvelope{Channel: "c", Payload: []byte("x")})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+PeerPublishPath, bytes.NewReader(body))
	req.Header.Set(peerSignatureHeader, signPeerBody([]byte("wrong-key"), body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
	if _, ok := tryRecvAfter(ch, 150*time.Millisecond); ok {
		t.Error("event fanned out despite bad signature")
	}
}

func TestCircuitBreaker(t *testing.T) {
	b := &circuitBreaker{}
	if !b.allow() {
		t.Fatal("fresh breaker should allow")
	}
	// Trip it.
	for range breakerFailThresh {
		b.record(false)
	}
	if b.allow() {
		t.Fatal("breaker should be open after consecutive failures")
	}
	// Simulate the cooldown elapsing → one probe allowed.
	b.mu.Lock()
	b.openedAt = time.Now().Add(-breakerCooldown - time.Second)
	b.mu.Unlock()
	if !b.allow() {
		t.Fatal("breaker should allow a probe after cooldown")
	}
	if b.allow() {
		t.Fatal("breaker should not allow a second concurrent probe")
	}
	// Probe succeeds → closed.
	b.record(true)
	if !b.allow() {
		t.Fatal("breaker should be closed after a successful probe")
	}
}

func tryRecvAfter(ch <-chan Event, d time.Duration) (Event, bool) {
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(d):
		return Event{}, false
	}
}
