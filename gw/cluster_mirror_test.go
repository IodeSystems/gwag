package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Event stream mirror between two clusters (cluster_mirror.go,
// event_mirror.go): a 3-node blue with the mirror listener and a
// 1-node green that sources blue's event streams over it.

const mirrorTestStream = "EV_orders_v1_OrderEvents"

// startMirrorBlue starts an n-node blue cluster, every node with a
// mirror listener, and a gateway on each. Returns the nodes and the
// mirror addresses.
func startMirrorBlue(t *testing.T, n int) ([]*clusterNode, []string) {
	t.Helper()
	routes := make([]string, n)
	mirrors := make([]string, n)
	for i := range routes {
		routes[i] = freeAddr(t)
		mirrors[i] = freeAddr(t)
	}
	nodes := make([]*clusterNode, n)
	for i := range nodes {
		var peers []string
		for j, r := range routes {
			if j != i {
				peers = append(peers, r)
			}
		}
		cl, err := StartCluster(ClusterOptions{
			NodeName:      fmt.Sprintf("blue%d", i),
			ClientListen:  freeAddr(t),
			ClusterListen: routes[i],
			Peers:         peers,
			DataDir:       t.TempDir(),
			ClusterName:   "blue",
			StartTimeout:  15 * time.Second,
			LogLevel:      "silent",
			MirrorListen:  mirrors[i],
		})
		if err != nil {
			t.Fatalf("blue%d: %v", i, err)
		}
		t.Cleanup(cl.Close)
		nodes[i] = &clusterNode{cluster: cl}
	}
	for i, nd := range nodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := nd.cluster.WaitForJetStream(ctx); err != nil {
			cancel()
			t.Fatalf("blue%d jetstream: %v", i, err)
		}
		cancel()
		g := New(WithCluster(nd.cluster), WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
		t.Cleanup(g.Close)
		nd.gw = g
		bootTracking(t, g)
	}
	return nodes, mirrors
}

// bootTracking runs startClusterTracking until the buckets exist.
func bootTracking(t *testing.T, g *Gateway) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := g.startClusterTracking(context.Background())
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("startClusterTracking: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startMirrorGreen starts a 1-node green cluster on dir whose source
// is from, with key, and a gateway on it.
func startMirrorGreen(t *testing.T, dir, from, key string) (*Cluster, *Gateway, func()) {
	t.Helper()
	cl, err := StartCluster(ClusterOptions{
		NodeName:      "green",
		ClientListen:  freeAddr(t),
		ClusterListen: freeAddr(t),
		DataDir:       dir,
		ClusterName:   "green",
		StartTimeout:  15 * time.Second,
		LogLevel:      "silent",
		MirrorFrom:    from,
		MirrorKey:     key,
	})
	if err != nil {
		t.Fatalf("green: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cl.WaitForJetStream(ctx); err != nil {
		cl.Close()
		t.Fatal(err)
	}
	g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")))
	bootTracking(t, g)
	var closed bool
	stop := func() {
		if !closed {
			closed = true
			g.Close()
			cl.Close()
		}
	}
	t.Cleanup(stop)
	return cl, g, stop
}

func publishEvents(t *testing.T, js jetstream.JetStream, ids ...string) {
	t.Helper()
	for _, id := range ids {
		m := nats.NewMsg("events.orders." + id + ".lifecycle")
		m.Header.Set(EventHeaderMsgID, id)
		m.Data = []byte("payload " + id)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := js.PublishMsg(ctx, m)
		cancel()
		if err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}
}

func streamMsgs(t *testing.T, js jetstream.JetStream, name string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, name)
	if err != nil {
		return 0
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0
	}
	return info.State.Msgs
}

// streamIDs is the Nats-Msg-Id of every message, in stream order.
func streamIDs(t *testing.T, js jetstream.JetStream, name string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && seq > 0; seq++ {
		m, err := s.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
		if got, want := m.Subject, "events.orders."+m.Header.Get(EventHeaderMsgID)+".lifecycle"; got != want {
			t.Errorf("seq %d subject %q, want %q", seq, got, want)
		}
		ids = append(ids, m.Header.Get(EventHeaderMsgID))
	}
	return ids
}

func seqIDs(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

func mirrorView1(t *testing.T, g *Gateway, name string) mirrorStreamView {
	t.Helper()
	v, err := g.mirrorStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range v.Streams {
		if s.Stream == name {
			return s
		}
	}
	return mirrorStreamView{}
}

// leafNodes is the number of leafnode connections over all servers.
func leafNodes(cls ...*Cluster) int {
	n := 0
	for _, c := range cls {
		n += c.Server.NumLeafNodes()
	}
	return n
}

// rawLeaf starts a bare NATS server whose leafnode remote is u.
func rawLeaf(t *testing.T, u string) *natsd.Server {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	s, err := natsd.NewServer(&natsd.Options{
		Host: "127.0.0.1", Port: natsd.RANDOM_PORT, NoSigs: true, NoLog: true,
		LeafNode: natsd.LeafNodeOpts{
			ReconnectInterval: 100 * time.Millisecond,
			Remotes:           []*natsd.RemoteLeafOpts{{URLs: []*url.URL{pu}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("raw leaf server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

func TestEventMirror(t *testing.T) {
	old := mirrorRecheckInterval
	mirrorRecheckInterval = 300 * time.Millisecond
	t.Cleanup(func() { mirrorRecheckInterval = old })

	blue, mirrors := startMirrorBlue(t, 3)
	blueCls := []*Cluster{blue[0].cluster, blue[1].cluster, blue[2].cluster}
	addOrdersProto(t, blue[0].gw)
	bjs := blue[1].cluster.JS
	waitGate(t, 20*time.Second, "blue stream", func() bool {
		_, err := bjs.Stream(context.Background(), mirrorTestStream)
		return err == nil
	})
	publishEvents(t, bjs, seqIDs("a", 1, 5)...)

	_, sysKey := mintSystemKey(t, blue[0].gw, time.Hour)
	_, appKey := mintKey(t, blue[0].gw, time.Hour, "orders")
	sysID := strings.SplitN(sysKey, "_", 3)[1]

	// The client port is as before: no credentials pass, others fail.
	nc, err := nats.Connect(blue[0].cluster.Server.ClientURL())
	if err != nil {
		t.Fatalf("client without credentials: %v", err)
	}
	nc.Close()
	if nc, err := nats.Connect(blue[0].cluster.Server.ClientURL(), nats.UserInfo(mirrorLeafUser, sysKey)); err == nil {
		nc.Close()
		t.Fatal("client with a system key as password: accepted on the client port")
	}

	// The mirror listener refuses no key, an app key and a wrong key.
	host := mirrors[0]
	for name, u := range map[string]string{
		"anonymous": "nats-leaf://" + host,
		"app key":   "nats-leaf://" + mirrorLeafUser + ":" + appKey + "@" + host,
		"wrong key": "nats-leaf://" + mirrorLeafUser + ":" + sysKey[:len(sysKey)-2] + "xx@" + host,
		"key token": "nats-leaf://" + appKey + "@" + host,
	} {
		rawLeaf(t, u)
		time.Sleep(time.Second)
		if n := leafNodes(blueCls...); n != 0 {
			t.Fatalf("%s: blue has %d leafnode connections, want 0", name, n)
		}
	}
	if _, err := StartCluster(ClusterOptions{DataDir: t.TempDir(), ClientListen: freeAddr(t), MirrorFrom: host, MirrorKey: appKey}); err == nil || !strings.Contains(err.Error(), "system key") {
		t.Fatalf("MirrorKey = app key: %v", err)
	}

	greenDir := t.TempDir()
	gcl, green, stopGreen := startMirrorGreen(t, greenDir, strings.Join(mirrors, ","), sysKey)
	waitGate(t, 20*time.Second, "green leafnode", func() bool { return gcl.Server.NumLeafNodes() == 1 })

	// Before start: blue's stream is listed, not mirrored.
	if s := mirrorView1(t, green, mirrorTestStream); !s.OnBlue || s.OnGreen || s.Mirrored || s.BlueLastSeq != 5 {
		t.Fatalf("status before start: %+v", s)
	}
	ctx := context.Background()
	if _, err := green.startMirror(ctx, "nats-leaf://elsewhere:1", nil); err == nil {
		t.Fatal("start with a --from that is not the source: accepted")
	}
	if _, err := green.startMirror(ctx, "", []string{"EV_nope"}); err == nil {
		t.Fatal("start for an unknown stream: accepted")
	}
	v, err := green.startMirror(ctx, mirrors[1], []string{"events.orders.*.lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Streams) == 0 {
		t.Fatalf("start: %+v", v)
	}
	gjs := gcl.JS
	waitGate(t, 20*time.Second, "5 messages on green", func() bool { return streamMsgs(t, gjs, mirrorTestStream) == 5 })
	if got := streamIDs(t, gjs, mirrorTestStream); strings.Join(got, ",") != strings.Join(seqIDs("a", 1, 5), ",") {
		t.Fatalf("green ids %v", got)
	}
	cfg := waitStream(t, gjs, mirrorTestStream, func(jetstream.StreamConfig) bool { return true })
	if cfg.MaxAge != defaultEventStreamMaxAge || cfg.Duplicates != eventStreamDuplicates || len(cfg.Subjects) != 1 || cfg.Subjects[0] != "events.orders.*.lifecycle" || !hasMirrorSource(cfg.Sources) {
		t.Fatalf("green stream config %+v", cfg)
	}
	// The other channels are not mirrored: only the listed one.
	for _, s := range v.Streams {
		if s.Stream != mirrorTestStream && s.Mirrored {
			t.Errorf("%s mirrored, not listed", s.Stream)
		}
	}

	// Green registers the same channel: the source stays.
	addOrdersProto(t, green)
	time.Sleep(3 * time.Second)
	if cfg := waitStream(t, gjs, mirrorTestStream, func(jetstream.StreamConfig) bool { return true }); !hasMirrorSource(cfg.Sources) {
		t.Fatalf("green's ensure dropped the source: %+v", cfg.Sources)
	}

	// More on blue, lag back to 0, sequences equal.
	publishEvents(t, bjs, seqIDs("a", 6, 10)...)
	var s mirrorStreamView
	waitGate(t, 20*time.Second, "status caught up", func() bool {
		s = mirrorView1(t, green, mirrorTestStream)
		return s.Mirrored && s.GreenMsgs == 10 && s.BlueLastSeq == 10 && s.Lag == 0 && s.ActiveMs >= 0
	})
	if s.Error != "" {
		t.Fatalf("source error: %+v", s)
	}

	// Dedup: the same Nats-Msg-Id published on green is a duplicate.
	m := nats.NewMsg("events.orders.a7.lifecycle")
	m.Header.Set(EventHeaderMsgID, "a7")
	ack, err := gjs.PublishMsg(ctx, m)
	if err != nil || !ack.Duplicate {
		t.Fatalf("publish a7 on green: %+v %v, want a duplicate", ack, err)
	}

	// Nothing else crosses: a core subject, both ways, and green's
	// own events stay on green.
	gsub, err := gcl.Conn.SubscribeSync("iso.test")
	if err != nil {
		t.Fatal(err)
	}
	bsub, err := blue[0].cluster.Conn.SubscribeSync("iso.test")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = blue[0].cluster.Conn.Publish("iso.test", []byte("blue"))
	_ = gcl.Conn.Publish("iso.test", []byte("green"))
	if m, err := gsub.NextMsg(time.Second); err != nil || string(m.Data) != "green" {
		t.Fatalf("green iso.test: %v %v", m, err)
	}
	if m, err := gsub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("blue's core message reached green: %q", m.Data)
	}
	if m, err := bsub.NextMsg(time.Second); err != nil || string(m.Data) != "blue" {
		t.Fatalf("blue iso.test: %v %v", m, err)
	}
	if m, err := bsub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("green's core message reached blue: %q", m.Data)
	}
	publishEvents(t, gjs, "g1")
	time.Sleep(time.Second)
	if n := streamMsgs(t, bjs, mirrorTestStream); n != 10 {
		t.Fatalf("blue has %d messages after a publish on green, want 10", n)
	}
	if n := streamMsgs(t, gjs, mirrorTestStream); n != 11 {
		t.Fatalf("green has %d messages, want 11", n)
	}

	// Green restarts; blue goes on meanwhile. It resumes with no
	// duplicate.
	stopGreen()
	publishEvents(t, bjs, seqIDs("a", 11, 13)...)
	gcl, green, _ = startMirrorGreen(t, greenDir, strings.Join(mirrors, ","), sysKey)
	gjs = gcl.JS
	waitGate(t, 30*time.Second, "14 messages after restart", func() bool { return streamMsgs(t, gjs, mirrorTestStream) == 14 })
	time.Sleep(2 * time.Second)
	want := append(append(seqIDs("a", 1, 10), "g1"), seqIDs("a", 11, 13)...)
	if got := streamIDs(t, gjs, mirrorTestStream); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("green ids after restart %v, want %v", got, want)
	}

	// Stop: the stream stays, with its messages, and no longer follows.
	v, err = green.stopMirror(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range v.Streams {
		if s.Mirrored {
			t.Fatalf("after stop: %+v", s)
		}
	}
	cfg = waitStream(t, gjs, mirrorTestStream, func(c jetstream.StreamConfig) bool { return len(c.Sources) == 0 })
	if cfg.MaxAge != defaultEventStreamMaxAge || cfg.Subjects[0] != "events.orders.*.lifecycle" {
		t.Fatalf("after stop: %+v", cfg)
	}
	publishEvents(t, bjs, "a14")
	time.Sleep(2 * time.Second)
	if n := streamMsgs(t, gjs, mirrorTestStream); n != 14 {
		t.Fatalf("green has %d messages after stop and a blue publish, want 14", n)
	}
	publishEvents(t, gjs, "g2")
	if n := streamMsgs(t, gjs, mirrorTestStream); n != 15 {
		t.Fatalf("green publish after stop: %d messages, want 15", n)
	}

	// Revoking the key ends the leafnode and keeps it out.
	if err := blue[0].gw.deleteAppKey(ctx, sysID); err != nil {
		t.Fatal(err)
	}
	waitGate(t, 10*time.Second, "leafnode closed after revoke", func() bool { return gcl.Server.NumLeafNodes() == 0 })
	time.Sleep(2 * time.Second)
	if n := leafNodes(append(blueCls, gcl)...); n != 0 {
		t.Fatalf("%d leafnode connections after revoke, want 0", n)
	}
}

// TestEventMirror_TwoNodeGreen: every green node has the leafnode;
// each blue message still lands once, and a stream the mirror creates
// gets green's replica count.
func TestEventMirror_TwoNodeGreen(t *testing.T) {
	blue, mirrors := startMirrorBlue(t, 1)
	addOrdersProto(t, blue[0].gw)
	bjs := blue[0].cluster.JS
	waitGate(t, 20*time.Second, "blue stream", func() bool {
		_, err := bjs.Stream(context.Background(), mirrorTestStream)
		return err == nil
	})
	_, sysKey := mintSystemKey(t, blue[0].gw, time.Hour)

	routes := []string{freeAddr(t), freeAddr(t)}
	var greens []*Gateway
	var gcls []*Cluster
	for i := range routes {
		cl, err := StartCluster(ClusterOptions{
			NodeName:      fmt.Sprintf("green%d", i),
			ClientListen:  freeAddr(t),
			ClusterListen: routes[i],
			Peers:         []string{routes[1-i]},
			DataDir:       t.TempDir(),
			ClusterName:   "green",
			StartTimeout:  15 * time.Second,
			LogLevel:      "silent",
			MirrorFrom:    mirrors[0],
			MirrorKey:     sysKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cl.Close)
		gcls = append(gcls, cl)
	}
	for _, cl := range gcls {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := cl.WaitForJetStream(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		g := New(WithCluster(cl), WithClusterReplicas(ClusterReplicasAuto), WithoutMetrics(), WithoutBackpressure())
		t.Cleanup(g.Close)
		bootTracking(t, g)
		greens = append(greens, g)
	}
	waitForPeers(t, greens[0], greens[1])
	waitGate(t, 20*time.Second, "both green leafnodes", func() bool { return leafNodes(gcls...) == 2 })

	if _, err := greens[1].startMirror(context.Background(), "", []string{mirrorTestStream}); err != nil {
		t.Fatal(err)
	}
	ids := seqIDs("b", 1, 20)
	publishEvents(t, bjs, ids...)
	gjs := gcls[0].JS
	waitGate(t, 20*time.Second, "20 messages on green", func() bool { return streamMsgs(t, gjs, mirrorTestStream) == 20 })
	time.Sleep(2 * time.Second)
	if got := streamIDs(t, gjs, mirrorTestStream); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("green ids %v", got)
	}
	if cfg := waitStream(t, gjs, mirrorTestStream, func(jetstream.StreamConfig) bool { return true }); cfg.Replicas != 2 {
		t.Fatalf("green stream replicas %d, want 2", cfg.Replicas)
	}
}

// mirrorTestTLS returns two mutual TLS configs for 127.0.0.1, each
// signed by its own fresh CA.
func mirrorTestTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	mk := func() *tls.Config {
		caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		ca := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mirror test ca"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
		caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		caCert, _ := x509.ParseCertificate(caDER)
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(caCert)
		return &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}
	}
	return mk(), mk()
}

// TestEventMirror_TLS: with MirrorTLS the leafnode needs a cert from
// the same CA; plain TCP and another CA do not connect.
func TestEventMirror_TLS(t *testing.T) {
	good, other := mirrorTestTLS(t)
	addr := freeAddr(t)
	cl, err := StartCluster(ClusterOptions{
		NodeName: "blue", ClientListen: freeAddr(t), ClusterListen: freeAddr(t), DataDir: t.TempDir(),
		LogLevel: "silent", MirrorListen: addr, MirrorTLS: good,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	g := New(WithCluster(cl), WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	bootTracking(t, g)
	_, key := mintSystemKey(t, g, time.Hour)
	green := func(cfg *tls.Config) *Cluster {
		c, err := StartCluster(ClusterOptions{
			NodeName: "green", ClientListen: freeAddr(t), ClusterListen: freeAddr(t), DataDir: t.TempDir(),
			LogLevel: "silent", MirrorFrom: addr, MirrorKey: key, MirrorTLS: cfg,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	green(nil)
	green(other)
	time.Sleep(2 * time.Second)
	if n := cl.Server.NumLeafNodes(); n != 0 {
		t.Fatalf("plain TCP or another CA: %d leafnodes, want 0", n)
	}
	gc := green(good)
	waitGate(t, 10*time.Second, "TLS leafnode", func() bool { return gc.Server.NumLeafNodes() == 1 })
}
