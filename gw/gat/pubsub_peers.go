package gat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// PeerPublishPath is the URL path (under the RegisterHTTP /
// RegisterHuma prefix) where a gat gateway accepts cross-node
// published events from its mesh peers. Internal — it is not part of
// the GraphQL schema.
//
// Stability: experimental
const PeerPublishPath = "/_gat/publish"

// peerSignatureHeader carries the hex HMAC-SHA256 of the request body,
// keyed by the mesh's shared secret.
const peerSignatureHeader = "X-Gat-Signature"

const (
	peerQueueDepth      = 256
	breakerFailThresh   = 5
	breakerCooldown     = 5 * time.Second
	peerPublishTimeout  = 3 * time.Second
)

// PeerMesh configures best-effort cross-node pubsub fanout. Peers is
// the set of peer gat base URLs (the RegisterHTTP / RegisterHuma
// prefix root, e.g. "https://gat-2.internal/api"); Auth is the shared
// HMAC secret every mesh member must hold. An empty Auth disables
// signature verification — only acceptable on a trusted network.
//
// Stability: experimental
type PeerMesh struct {
	Peers []string
	Auth  []byte
}

// EnablePeerMesh turns on best-effort cross-node fanout: it is
// UseBus(NewPeerMeshBus(m)) plus mounting the PeerPublishPath receive
// endpoint on RegisterHTTP / RegisterHuma. After this,
// PubSub().Publish delivers locally and also fire-and-forgets the
// event to every peer; each peer's receive handler fans it out
// locally only (one hop — no re-broadcast). Call once, before
// serving traffic; Gateway.Close stops the peer goroutines. A later
// UseBus replaces the mesh bus.
//
// Stability: experimental
func (g *Gateway) EnablePeerMesh(m PeerMesh) {
	mb := NewPeerMeshBus(m)
	g.mesh = mb
	g.pubsub.setBus(mb)
}

// PeerMeshBus is the Bus behind EnablePeerMesh: local delivery plus a
// best-effort HTTP POST of every published event to each peer. Events
// received from peers (Handler) are delivered locally only, which
// caps fanout at one hop. Each peer has a bounded queue (drop when
// full) and a circuit breaker.
//
// Stability: experimental
type PeerMeshBus struct {
	local *LocalBus
	mesh  *peerMesh
}

var _ Bus = (*PeerMeshBus)(nil)

// NewPeerMeshBus starts one sender goroutine per peer. Close stops
// them.
//
// Stability: experimental
func NewPeerMeshBus(m PeerMesh) *PeerMeshBus {
	mesh := &peerMesh{
		auth:   m.Auth,
		client: &http.Client{Timeout: peerPublishTimeout},
		peers:  make([]*peerConn, 0, len(m.Peers)),
	}
	for _, url := range m.Peers {
		pc := &peerConn{
			url:     url + PeerPublishPath,
			queue:   make(chan peerEnvelope, peerQueueDepth),
			breaker: &circuitBreaker{},
		}
		mesh.peers = append(mesh.peers, pc)
		mesh.wg.Add(1)
		go pc.run(mesh.client, mesh.auth, &mesh.wg)
	}
	return &PeerMeshBus{local: NewLocalBus(), mesh: mesh}
}

// Publish delivers locally and enqueues the event for every peer.
//
// Stability: experimental
func (b *PeerMeshBus) Publish(channel string, payload []byte) error {
	if err := b.local.Publish(channel, payload); err != nil {
		return err
	}
	b.mesh.fanout(channel, payload)
	return nil
}

// Subscribe registers a local subscription; peers deliver into it
// through Handler.
//
// Stability: experimental
func (b *PeerMeshBus) Subscribe(pattern string, fn func(channel string, payload []byte)) (func(), error) {
	return b.local.Subscribe(pattern, fn)
}

// Close stops the peer sender goroutines after they drain their
// queues.
//
// Stability: experimental
func (b *PeerMeshBus) Close() {
	b.mesh.stop()
}

// peerEnvelope is the wire shape POSTed between mesh peers. Payload
// JSON-encodes as base64.
type peerEnvelope struct {
	Channel string `json:"channel"`
	Payload []byte `json:"payload"`
}

type peerMesh struct {
	auth   []byte
	client *http.Client
	peers  []*peerConn
	wg     sync.WaitGroup
}

// fanout enqueues an event for every peer, non-blocking. A full
// per-peer queue drops the event for that peer (best-effort) so a
// slow or dead peer never stalls the publisher.
func (m *peerMesh) fanout(channel string, payload []byte) {
	ev := peerEnvelope{Channel: channel, Payload: payload}
	for _, pc := range m.peers {
		select {
		case pc.queue <- ev:
		default: // queue full — drop for this peer
		}
	}
}

// stop closes every peer queue and waits for the sender goroutines
// to drain and exit.
func (m *peerMesh) stop() {
	for _, pc := range m.peers {
		close(pc.queue)
	}
	m.wg.Wait()
}

// peerConn is one mesh peer: a bounded send queue drained by a single
// goroutine, gated by a circuit breaker.
type peerConn struct {
	url     string
	queue   chan peerEnvelope
	breaker *circuitBreaker
}

// run drains the peer's queue, POSTing each event unless the breaker
// is open. Exits when the queue is closed (Gateway.Close).
func (pc *peerConn) run(client *http.Client, auth []byte, wg *sync.WaitGroup) {
	defer wg.Done()
	for ev := range pc.queue {
		if !pc.breaker.allow() {
			continue // breaker open — drop, don't probe with real traffic
		}
		err := postPeerEvent(client, pc.url, auth, ev)
		pc.breaker.record(err == nil)
	}
}

// postPeerEvent POSTs one event to a peer, signing the body with the
// mesh HMAC secret.
func postPeerEvent(client *http.Client, url string, auth []byte, ev peerEnvelope) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(auth) > 0 {
		req.Header.Set(peerSignatureHeader, signPeerBody(auth, body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return &peerStatusError{code: resp.StatusCode}
	}
	return nil
}

type peerStatusError struct{ code int }

func (e *peerStatusError) Error() string {
	return "gat: peer publish rejected with status " + http.StatusText(e.code)
}

// signPeerBody returns the hex HMAC-SHA256 of body under key.
func signPeerBody(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyPeerBody constant-time-checks a hex HMAC signature against the
// recomputed one. An empty key means "no verification configured" and
// always passes — the operator opted out.
func verifyPeerBody(key, body []byte, sigHex string) bool {
	if len(key) == 0 {
		return true
	}
	want := signPeerBody(key, body)
	return hmac.Equal([]byte(want), []byte(sigHex))
}

// peerPublishHandler returns the mesh receive handler for
// PeerPublishPath, or 404 when no mesh is enabled.
func (g *Gateway) peerPublishHandler() http.Handler {
	if g.mesh == nil {
		return http.NotFoundHandler()
	}
	return g.mesh.Handler()
}

// Handler is the http.Handler for PeerPublishPath: it verifies the
// HMAC, decodes the envelope, and delivers the event LOCAL-ONLY. Not
// re-sending to peers is the loop guard that caps fanout at one hop.
//
// Stability: experimental
func (b *PeerMeshBus) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := readAllLimited(r, 1<<20)
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !verifyPeerBody(b.mesh.auth, body, r.Header.Get(peerSignatureHeader)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var ev peerEnvelope
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "invalid envelope", http.StatusBadRequest)
			return
		}
		_ = b.local.Publish(ev.Channel, ev.Payload)
		w.WriteHeader(http.StatusNoContent)
	})
}

// readAllLimited reads the request body up to max bytes, erroring if
// the body would exceed it.
func readAllLimited(r *http.Request, max int64) ([]byte, error) {
	buf := &bytes.Buffer{}
	n, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, max))
	if err != nil {
		return nil, err
	}
	_ = n
	return buf.Bytes(), nil
}

// circuitBreaker is a minimal per-peer breaker: it opens after
// breakerFailThresh consecutive failures and stays open for
// breakerCooldown, after which one probe is allowed (half-open). A
// success closes it; a failure re-opens it.
type circuitBreaker struct {
	mu           sync.Mutex
	failures     int
	openedAt     time.Time
	open         bool
	probing      bool
}

// allow reports whether a send may proceed. When open, it returns
// false until the cooldown elapses, then allows a single probe.
func (b *circuitBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if b.probing {
		return false // a probe is already in flight
	}
	if time.Since(b.openedAt) >= breakerCooldown {
		b.probing = true
		return true
	}
	return false
}

// record updates the breaker with the outcome of a send.
func (b *circuitBreaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if ok {
		b.failures = 0
		b.open = false
		return
	}
	b.failures++
	if b.failures >= breakerFailThresh {
		b.open = true
		b.openedAt = time.Now()
	}
}
