package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Cluster wraps an embedded NATS server, a JetStream context, and a
// connection back to the local server. Multiple gateways form a cluster
// by pointing their ClusterOptions.Peers at one another's cluster routes.
type Cluster struct {
	Server *natsd.Server
	Conn   *nats.Conn
	JS     jetstream.JetStream

	// NodeID is the NATS server's stable identifier within the cluster
	// — used as the gateway's identity in the peers KV bucket.
	NodeID string

	// Environment is the deployment label this cluster runs under
	// (empty string when no --environment was set). Surfaced in the
	// schema endpoint headers and operator listings.
	Environment string
}

// ClusterOptions configures the embedded NATS server. ClientListen is
// where local gateway code (and external services, if exposed) talk
// JetStream; ClusterListen is the inter-node route. Peers is the list
// of well-known cluster routes to dial on startup; the rest of the
// cluster is learned via NATS gossip.
type ClusterOptions struct {
	NodeName      string
	ClientListen  string // e.g. ":14222"; default ":14222"
	ClusterListen string // e.g. ":14248"; default ":14248"
	Peers         []string
	DataDir       string // JetStream storage; required for persistence

	// Environment is a deployment-time label (e.g. "dev", "prod"). It
	// becomes part of the NATS cluster name so two clusters in the
	// same network with different envs cannot federate. Empty keeps
	// the legacy default cluster name "go-api-gateway".
	Environment string

	// StartTimeout caps how long we wait for the server to be ready.
	StartTimeout time.Duration

	Debug bool
	Trace bool

	// TLS, when non-nil, enables mTLS on cluster routes. Both the cert
	// pool used for verifying peers and the server cert+key must be
	// configured. ClientAuth=RequireAndVerifyClientCert is recommended
	// for true mTLS; callers can lower it for one-way TLS.
	TLS *tls.Config
}

// StartCluster boots an embedded NATS server with JetStream enabled and
// returns a Cluster bound to it. Callers are responsible for Close().
func StartCluster(opts ClusterOptions) (*Cluster, error) {
	if opts.ClientListen == "" {
		opts.ClientListen = ":14222"
	}
	if opts.ClusterListen == "" {
		opts.ClusterListen = ":14248"
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 10 * time.Second
	}
	if opts.DataDir == "" {
		return nil, errors.New("cluster: DataDir is required for persistence")
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("cluster: resolve DataDir: %w", err)
	}

	host, port, err := splitHostPort(opts.ClientListen)
	if err != nil {
		return nil, fmt.Errorf("cluster: ClientListen: %w", err)
	}

	srvOpts := &natsd.Options{
		ServerName:     opts.NodeName,
		Host:           host,
		Port:           port,
		JetStream:      true,
		StoreDir:       dataDir,
		NoSigs:         true,
		MaxControlLine: 4096,
		Debug:          opts.Debug,
		Trace:          opts.Trace,
	}

	// Cluster mode only when peers are configured. A standalone seed
	// runs as single-node JetStream (R=1). To later scale beyond one
	// node, every node — including the seed — must be (re)started with
	// at least one --nats-peer entry.
	if len(opts.Peers) > 0 {
		cHost, cPort, err := splitHostPort(opts.ClusterListen)
		if err != nil {
			return nil, fmt.Errorf("cluster: ClusterListen: %w", err)
		}
		routes, err := parseRouteURLs(opts.Peers)
		if err != nil {
			return nil, fmt.Errorf("cluster: Peers: %w", err)
		}
		clusterName := "go-api-gateway"
		if opts.Environment != "" {
			clusterName = clusterName + "-" + opts.Environment
		}
		srvOpts.Cluster = natsd.ClusterOpts{
			Name: clusterName,
			Host: cHost,
			Port: cPort,
		}
		if opts.TLS != nil {
			srvOpts.Cluster.TLSConfig = opts.TLS.Clone()
			srvOpts.Cluster.TLSTimeout = 5
		}
		srvOpts.Routes = routes
	}

	srv, err := natsd.NewServer(srvOpts)
	if err != nil {
		return nil, fmt.Errorf("cluster: NewServer: %w", err)
	}
	srv.ConfigureLogger()
	go srv.Start()
	if !srv.ReadyForConnections(opts.StartTimeout) {
		srv.Shutdown()
		return nil, fmt.Errorf("cluster: server not ready after %s", opts.StartTimeout)
	}

	conn, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("cluster: connect: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		srv.Shutdown()
		return nil, fmt.Errorf("cluster: jetstream: %w", err)
	}

	return &Cluster{
		Server:      srv,
		Conn:        conn,
		JS:          js,
		NodeID:      srv.ID(),
		Environment: opts.Environment,
	}, nil
}

// Close drains the connection and shuts the server down.
func (c *Cluster) Close() {
	if c == nil {
		return
	}
	if c.Conn != nil {
		c.Conn.Close()
	}
	if c.Server != nil {
		c.Server.Shutdown()
		c.Server.WaitForShutdown()
	}
}

// WaitForJetStream blocks until JetStream reports ready (relevant in
// freshly-formed clusters where stream meta needs to settle).
func (c *Cluster) WaitForJetStream(ctx context.Context) error {
	deadline, _ := ctx.Deadline()
	for {
		if c.Server.JetStreamIsLeader() || c.Server.JetStreamIsCurrent() {
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return errors.New("cluster: jetstream not ready before deadline")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// LoadMTLSConfig builds a *tls.Config that requires and verifies a
// client cert against caFile, presenting (certFile, keyFile) as the
// server identity. The same config is suitable for both NATS cluster
// routes and the gateway's gRPC control plane — for true mesh mTLS,
// use a single CA across the deployment and issue one (cert,key) pair
// per node.
//
// Pass nil paths to bail out — callers convert "" flags to nil.
func LoadMTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, errors.New("LoadMTLSConfig: certFile, keyFile, caFile all required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ca %s contains no valid certs", caFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// parseRouteURLs accepts entries like "nats-route://host:port" or
// "host:port" (sugar) and returns parsed URLs.
func parseRouteURLs(peers []string) ([]*url.URL, error) {
	if len(peers) == 0 {
		return nil, nil
	}
	out := make([]*url.URL, 0, len(peers))
	for _, p := range peers {
		raw := p
		if !hasScheme(raw) {
			raw = "nats-route://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %w", p, err)
		}
		out = append(out, u)
	}
	return out, nil
}

func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ':':
			return i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/'
		case '/':
			return false
		}
	}
	return false
}

func splitHostPort(addr string) (string, int, error) {
	host, portStr := "", addr
	if i := lastIndexByte(addr, ':'); i >= 0 {
		host, portStr = addr[:i], addr[i+1:]
	}
	if portStr == "" {
		return "", 0, fmt.Errorf("missing port in %q", addr)
	}
	port := 0
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return "", 0, fmt.Errorf("non-numeric port in %q", addr)
		}
		port = port*10 + int(r-'0')
	}
	return host, port, nil
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
