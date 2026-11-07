//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// GatewayOpts configures one gateway process
// (examples/multi/cmd/gateway). The zero value is a single node with
// no NATS.
type GatewayOpts struct {
	// Name is the process name and NATS node name. Default "gw", or
	// "n1".."nN" in a cluster.
	Name string
	// NATS embeds a NATS server (needed for subscriptions). Cluster
	// nodes always have it.
	NATS bool
	// Environment is the --environment label.
	Environment string
	// NoCallerIDHeader leaves --caller-id-public off: the gateway then
	// ignores X-Caller-Id and every caller is "unknown".
	NoCallerIDHeader bool
	// Args are extra gateway flags, e.g. "--max-inflight", "4".
	Args []string
	// StartTimeout bounds the wait for /api/health and the control
	// plane. Default 30s.
	StartTimeout time.Duration
}

// Gateway is a running gateway process.
type Gateway struct {
	Name     string
	HTTPPort int
	CPPort   int
	// NATSClientPort and NATSClusterPort are 0 without NATS.
	NATSClientPort  int
	NATSClusterPort int
	// DataDir holds the admin token (and JetStream with NATS).
	DataDir string
	Proc    *Proc

	env  *Env
	opts GatewayOpts

	mu     sync.Mutex
	token  string
	cpConn *grpc.ClientConn
	http   *http.Client
}

// BaseURL is the gateway's HTTP root, e.g. http://127.0.0.1:PORT.
func (g *Gateway) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", g.HTTPPort) }

// CPAddr is the control-plane (and native gRPC ingress) address.
func (g *Gateway) CPAddr() string { return fmt.Sprintf("127.0.0.1:%d", g.CPPort) }

// NATSRoute is this node's NATS cluster route address.
func (g *Gateway) NATSRoute() string { return fmt.Sprintf("127.0.0.1:%d", g.NATSClusterPort) }

// Gateway starts one gateway and waits until it serves.
func (e *Env) Gateway(opts GatewayOpts) *Gateway {
	e.t.Helper()
	if opts.Name == "" {
		opts.Name = "gw"
	}
	g := e.newGateway(opts)
	g.start(nil)
	return g
}

func (e *Env) newGateway(opts GatewayOpts) *Gateway {
	g := &Gateway{
		Name:     opts.Name,
		HTTPPort: FreePort(e.t),
		CPPort:   FreePort(e.t),
		DataDir:  filepath.Join(e.dir, opts.Name),
		env:      e,
		opts:     opts,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
	if opts.NATS {
		g.NATSClientPort = FreePort(e.t)
		g.NATSClusterPort = FreePort(e.t)
	}
	if err := os.MkdirAll(g.DataDir, 0o755); err != nil {
		e.t.Fatalf("harness: %v", err)
	}
	return g
}

func (g *Gateway) start(peers []string) {
	g.env.t.Helper()
	g.Proc = g.env.Start(g.Name, Binary(g.env.t, "gateway"), g.args(peers)...)
	g.WaitReady()
}

// args is the gateway command line. peers are NATS cluster routes.
func (g *Gateway) args(peers []string) []string {
	// --http must be ":PORT": the gateway self-ingests its admin
	// OpenAPI at "http://localhost"+<http flag>.
	args := []string{
		"--http", fmt.Sprintf(":%d", g.HTTPPort),
		"--control-plane", g.CPAddr(),
		"--admin-data-dir", g.DataDir,
	}
	if g.opts.NATS {
		args = append(args,
			"--nats-data", filepath.Join(g.DataDir, "nats"),
			"--nats-listen", fmt.Sprintf("127.0.0.1:%d", g.NATSClientPort),
			"--nats-cluster", g.NATSRoute(),
			"--node-name", g.Name,
		)
		for _, p := range peers {
			args = append(args, "--nats-peer", p)
		}
	}
	if !g.opts.NoCallerIDHeader {
		args = append(args, "--caller-id-public")
	}
	if g.opts.Environment != "" {
		args = append(args, "--environment", g.opts.Environment)
	}
	return append(args, g.opts.Args...)
}

// WaitReady waits until /api/health answers 200 and the control plane
// accepts connections, then reads the admin token.
func (g *Gateway) WaitReady() {
	t := g.env.t
	t.Helper()
	timeout := g.opts.StartTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	Eventually(t, timeout, g.Name+" serves /api/health and the control plane", func() error {
		if !g.Proc.Running() {
			code, _ := g.Proc.ExitCode()
			return Permanent(fmt.Errorf("%s exited with code %d", g.Name, code))
		}
		resp, err := g.http.Get(g.BaseURL() + "/api/health")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("/api/health: HTTP %d", resp.StatusCode)
		}
		c, err := net.DialTimeout("tcp", g.CPAddr(), time.Second)
		if err != nil {
			return err
		}
		c.Close()
		return nil
	})
	b, err := os.ReadFile(filepath.Join(g.DataDir, "admin-token"))
	if err != nil {
		t.Fatalf("harness: %s admin token: %v", g.Name, err)
	}
	g.mu.Lock()
	g.token = strings.TrimSpace(string(b))
	g.mu.Unlock()
}

// Token returns the admin bearer token (boot token persisted under
// DataDir).
func (g *Gateway) Token() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.token
}

// Restart stops the gateway with SIGTERM (it drains), starts it again
// on the same ports and data directory, and waits until it serves.
func (g *Gateway) Restart() {
	g.env.t.Helper()
	g.Proc.Restart()
	g.WaitReady()
}

// Health returns the decoded /api/health body and HTTP status.
func (g *Gateway) Health(ctx context.Context) (map[string]any, int, error) {
	var out map[string]any
	status, err := g.do(ctx, http.MethodGet, "/api/health", nil, &out, nil)
	return out, status, err
}

// ControlPlane returns a client for the gateway's control plane
// (Register, DrainInstance, ListRegistrations, ...). The connection is
// shared and closed when the test ends.
func (g *Gateway) ControlPlane() cpv1.ControlPlaneClient {
	return cpv1.NewControlPlaneClient(g.grpcConn())
}

func (g *Gateway) grpcConn() *grpc.ClientConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cpConn == nil {
		c, err := grpc.NewClient(g.CPAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			g.env.t.Fatalf("harness: dial %s: %v", g.CPAddr(), err)
		}
		g.cpConn = c
		g.env.t.Cleanup(func() { _ = c.Close() })
	}
	return g.cpConn
}

// ---- cluster --------------------------------------------------------

// Cluster is a set of gateways joined over embedded NATS.
type Cluster struct {
	Nodes []*Gateway
	env   *Env
	opts  GatewayOpts
}

// Cluster starts n gateways (n1..nN) that list each other as NATS
// peers, and waits until every node sees all n peers. opts applies to
// every node; Name is ignored.
func (e *Env) Cluster(n int, opts GatewayOpts) *Cluster {
	e.t.Helper()
	c := &Cluster{env: e, opts: opts}
	for i := 1; i <= n; i++ {
		o := opts
		o.Name = "n" + strconv.Itoa(i)
		o.NATS = true
		c.Nodes = append(c.Nodes, e.newGateway(o))
	}
	// Start every node before waiting on any: a node in cluster mode
	// may not serve until its peers' routes answer.
	for _, g := range c.Nodes {
		g.Proc = e.Start(g.Name, Binary(e.t, "gateway"), g.args(c.peersOf(g))...)
	}
	for _, g := range c.Nodes {
		g.WaitReady()
	}
	c.WaitPeers(n, 60*time.Second)
	return c
}

func (c *Cluster) peersOf(g *Gateway) []string {
	var out []string
	for _, o := range c.Nodes {
		if o != g {
			out = append(out, o.NATSRoute())
		}
	}
	return out
}

// AddNode starts one more node peered with every current node and
// waits until all nodes see each other.
func (c *Cluster) AddNode() *Gateway {
	e := c.env
	e.t.Helper()
	o := c.opts
	o.Name = "n" + strconv.Itoa(len(c.Nodes)+1)
	o.NATS = true
	g := e.newGateway(o)
	c.Nodes = append(c.Nodes, g)
	g.start(c.peersOf(g))
	c.WaitPeers(len(c.Nodes), 60*time.Second)
	return g
}

// Live returns the nodes whose process is running.
func (c *Cluster) Live() []*Gateway {
	var out []*Gateway
	for _, g := range c.Nodes {
		if g.Proc.Running() {
			out = append(out, g)
		}
	}
	return out
}

// WaitPeers waits until every running node lists want peers in
// /api/admin/peers.
func (c *Cluster) WaitPeers(want int, timeout time.Duration) {
	c.env.t.Helper()
	Eventually(c.env.t, timeout, fmt.Sprintf("every live node sees %d peers", want), func() error {
		for _, g := range c.Live() {
			var out struct {
				Peers []struct {
					NodeID string `json:"nodeId"`
					Name   string `json:"name"`
				} `json:"peers"`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := g.do(ctx, http.MethodGet, "/api/admin/peers", nil, &out, nil)
			cancel()
			if err != nil {
				return fmt.Errorf("%s: %w", g.Name, err)
			}
			if len(out.Peers) != want {
				b, _ := json.Marshal(out.Peers)
				return fmt.Errorf("%s sees %d peers: %s", g.Name, len(out.Peers), b)
			}
		}
		return nil
	})
}
