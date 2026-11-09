package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Event stream mirror between clusters (cutover S7, docs/cutover.md).
//
// The NATS side. Blue (ClusterOptions.MirrorListen) opens a leafnode
// listener. Green (MirrorFrom, MirrorKey) connects to it as a
// leafnode, bound on both sides to the global account ($G, where the
// EV_* streams are). Green's EV_* streams then source blue's with
// JetStream stream sourcing (StreamSource.External), so the server
// does the copying, the resume after a restart and the Nats-Msg-Id
// dedup; gwag only edits stream configs (event_mirror.go).
//
// Nothing else crosses the leafnode. Blue binds the connection to a
// user whose permissions allow only:
//
//   - green to blue: mirrorAPIPrefix (the JetStream API, reached by
//     the account mappings in mirrorMappings), and $JS.FC.> (flow
//     control replies of the sourcing consumers);
//   - blue to green: mirrorDeliverPrefix (the sourcing consumers'
//     deliveries), $JSC.R.> (replies to the consumer creates), and
//     mirrorInboxPrefix (replies to green's status reads).
//
// So no event published on one side reaches the other's streams or
// subscribers directly, and the gateways' KV buckets ($KV.>) stay
// apart. The server adds its own deny of $JS.API.>, $KV.> and $OBJ.>
// on a leafnode between two JetStream servers of the same (empty)
// domain. No JetStream domain is set: a domain on an existing cluster
// would mean a restart of every node into a new domain, and the
// mappings give the same reach.
//
// Auth. A leafnode connection to blue must carry a system key of
// blue (gws_, the URL password or token). With MirrorListen set, blue
// installs a CustomClientAuthentication (nats-server calls it for
// client connections and for leafnodes without leafnode users):
//
//   - leafnode: the key is checked like any system key
//     (checkSystemKey, through the gateway bound to this cluster);
//     the connection ends at the key's expiry, and a key revoked
//     later ends it at the next recheck (mirrorRecheckInterval);
//   - client: the same as without the mirror. Standalone: no auth.
//     Cluster: the in-process system user, or no credentials ($G);
//     anything else is refused.
//
// Without MirrorListen nothing changes: no custom authentication.

const (
	// mirrorAPIPrefix is the JetStream API prefix green's stream
	// sources use (ExternalStream.APIPrefix).
	mirrorAPIPrefix = "$GWAG.MIRROR.API"
	// mirrorDeliverPrefix is the deliver subject prefix of the
	// sourcing consumers on blue (ExternalStream.DeliverPrefix).
	mirrorDeliverPrefix = "$GWAG.MIRROR.DELIVER"
	// mirrorInboxPrefix is the inbox of green's reads of blue's
	// streams (status).
	mirrorInboxPrefix = "$GWAG.MIRROR.INBOX"
	// mirrorLeafUser is the user name green sends with the key, and
	// the prefix of the user blue binds the connection to.
	mirrorLeafUser = "gwag-mirror"
	// mirrorAuthTimeout bounds the key check in the auth callback.
	mirrorAuthTimeout = 5 * time.Second
)

// mirrorRecheckInterval is how often blue checks the keys of live
// mirror connections again. A var for tests.
var mirrorRecheckInterval = 30 * time.Second

// mirrorMappings are the account mappings blue adds to $G: the parts
// of the JetStream API stream sourcing and the status read use.
// CONSUMER.DELETE is not mapped: a key holder could delete any
// consumer. A sourcing consumer on blue is removed by its inactive
// threshold (10 s on nats-server v2.15.2) instead.
var mirrorMappings = map[string]string{
	mirrorAPIPrefix + ".CONSUMER.CREATE.>": "$JS.API.CONSUMER.CREATE.>",
	mirrorAPIPrefix + ".STREAM.LIST":       "$JS.API.STREAM.LIST",
}

// mirrorLeafPermissions are the hub-side permissions of a mirror
// leafnode. On a hub, Publish is what the leafnode may send in and
// Subscribe what the hub sends out.
func mirrorLeafPermissions() *natsd.Permissions {
	return &natsd.Permissions{
		Publish: &natsd.SubjectPermission{Allow: []string{
			mirrorAPIPrefix + ".>",
			"$JS.FC.>",
		}},
		Subscribe: &natsd.SubjectPermission{Allow: []string{
			mirrorDeliverPrefix + ".>",
			"$JSC.R.>",
			mirrorInboxPrefix + ".>",
		}},
	}
}

// mirrorKeyCheck validates a system key: its id and expiry.
type mirrorKeyCheck func(ctx context.Context, key string) (id string, expires time.Time, err error)

type mirrorAuth struct {
	owner *Gateway
	check mirrorKeyCheck
}

// clusterMirror is a Cluster's mirror state.
type clusterMirror struct {
	// listen: this server has the mirror listener (blue).
	listen bool
	// from is green's source, password removed; empty on blue only.
	from    []*url.URL
	sysUser *natsd.User // nil when standalone

	srv  *natsd.Server
	auth atomic.Pointer[mirrorAuth]

	mu sync.Mutex
	// leafs are the live mirror connections: client id → key.
	leafs map[uint64]string
	// blue is green's connection for reads of blue's streams.
	blue   *nats.Conn
	blueJS jetstream.JetStream
}

// configureMirror applies MirrorListen / MirrorFrom to srvOpts.
func configureMirror(srvOpts *natsd.Options, opts ClusterOptions, sysUser *natsd.User) (*clusterMirror, error) {
	if opts.MirrorListen == "" && opts.MirrorFrom == "" {
		if opts.MirrorKey != "" {
			return nil, errors.New("cluster: MirrorKey needs MirrorFrom")
		}
		return nil, nil
	}
	m := &clusterMirror{sysUser: sysUser, leafs: map[uint64]string{}}
	if opts.MirrorListen != "" {
		host, port, err := splitHostPort(opts.MirrorListen)
		if err != nil {
			return nil, fmt.Errorf("cluster: MirrorListen: %w", err)
		}
		if port == 0 {
			port = natsd.RANDOM_PORT
		}
		srvOpts.LeafNode.Host = host
		srvOpts.LeafNode.Port = port
		if opts.MirrorTLS != nil {
			srvOpts.LeafNode.TLSConfig = opts.MirrorTLS.Clone()
			srvOpts.LeafNode.TLSTimeout = 5
		}
		srvOpts.CustomClientAuthentication = m
		m.listen = true
	}
	if opts.MirrorFrom != "" {
		if _, ok := parseSystemKey(opts.MirrorKey); !ok {
			return nil, errors.New("cluster: MirrorKey must be a system key (gws_...) minted on the source cluster")
		}
		urls, err := parseMirrorURLs(opts.MirrorFrom)
		if err != nil {
			return nil, fmt.Errorf("cluster: MirrorFrom: %w", err)
		}
		withKey := make([]*url.URL, len(urls))
		for i, u := range urls {
			k := *u
			k.User = url.UserPassword(mirrorLeafUser, opts.MirrorKey)
			withKey[i] = &k
		}
		remote := &natsd.RemoteLeafOpts{URLs: withKey}
		if opts.MirrorTLS != nil {
			remote.TLS = true
			remote.TLSConfig = opts.MirrorTLS.Clone()
			remote.TLSTimeout = 5
		}
		srvOpts.LeafNode.Remotes = append(srvOpts.LeafNode.Remotes, remote)
		m.from = urls
	}
	return m, nil
}

// parseMirrorURLs reads MirrorFrom: comma-separated host:port or
// URLs; no scheme means nats-leaf://. Credentials in a URL are
// refused (the key is MirrorKey).
func parseMirrorURLs(s string) ([]*url.URL, error) {
	var out []*url.URL
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw := p
		if !hasScheme(raw) {
			raw = "nats-leaf://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", p, err)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%q: no host", p)
		}
		if u.User != nil {
			return nil, fmt.Errorf("%q: credentials in the URL; the system key goes in MirrorKey", p)
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, errors.New("no address")
	}
	return out, nil
}

// bind records the server, before it starts: Check needs it.
func (m *clusterMirror) bind(srv *natsd.Server) {
	if m != nil {
		m.srv = srv
	}
}

// started adds blue's account mappings once the server is up. No
// leafnode passes auth before a gateway binds (setAuth), which is
// later.
func (m *clusterMirror) started() error {
	if m == nil || !m.listen {
		return nil
	}
	acc := m.srv.GlobalAccount()
	for src, dst := range mirrorMappings {
		if err := acc.AddMapping(src, dst); err != nil {
			return fmt.Errorf("cluster: mirror mapping %s: %w", src, err)
		}
	}
	return nil
}

func (m *clusterMirror) close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blue != nil {
		m.blue.Close()
		m.blue, m.blueJS = nil, nil
	}
}

// setAuth makes g's system keys the credential of the mirror
// listener. The last gateway bound wins; clearAuth(g) undoes it.
func (m *clusterMirror) setAuth(g *Gateway, check mirrorKeyCheck) {
	m.auth.Store(&mirrorAuth{owner: g, check: check})
}

func (m *clusterMirror) clearAuth(g *Gateway) {
	if a := m.auth.Load(); a != nil && a.owner == g {
		m.auth.CompareAndSwap(a, nil)
	}
}

// Check implements natsd.Authentication.
func (m *clusterMirror) Check(c natsd.ClientAuthentication) bool {
	o := c.GetOpts()
	switch c.Kind() {
	case natsd.CLIENT:
		return m.checkClient(c, o)
	case natsd.LEAF:
		return m.checkLeaf(c, o)
	}
	return false
}

// checkClient is the client auth StartCluster has without the
// mirror (see the package comment above).
func (m *clusterMirror) checkClient(c natsd.ClientAuthentication, o *natsd.ClientOpts) bool {
	if m.sysUser == nil {
		return true
	}
	if o.Username == m.sysUser.Username {
		if subtle.ConstantTimeCompare([]byte(o.Password), []byte(m.sysUser.Password)) != 1 {
			return false
		}
		sys := m.srv.SystemAccount()
		if sys == nil {
			return false
		}
		c.RegisterUser(&natsd.User{Username: m.sysUser.Username, Account: sys})
		return true
	}
	return o.Username == "" && o.Password == "" && o.Token == "" && o.Nkey == "" && o.JWT == ""
}

func (m *clusterMirror) checkLeaf(c natsd.ClientAuthentication, o *natsd.ClientOpts) bool {
	if !m.listen {
		return false
	}
	a := m.auth.Load()
	if a == nil {
		return false
	}
	key := o.Password
	if key == "" {
		key = o.Token
	}
	ctx, cancel := context.WithTimeout(context.Background(), mirrorAuthTimeout)
	defer cancel()
	id, expires, err := a.check(ctx, key)
	if err != nil {
		m.srv.Warnf("gateway: event stream mirror: leafnode from %s refused: %v", c.RemoteAddress(), err)
		return false
	}
	c.RegisterUser(&natsd.User{
		Username:           mirrorLeafUser + ":" + id,
		Account:            m.srv.GlobalAccount(),
		Permissions:        mirrorLeafPermissions(),
		ConnectionDeadline: expires,
	})
	m.mu.Lock()
	m.leafs[c.GetID()] = key
	m.mu.Unlock()
	return true
}

// recheck ends the live mirror connections whose key is no longer
// valid (revoked; expiry is the connection deadline already). A key
// that cannot be checked now keeps its connection.
func (m *clusterMirror) recheck(ctx context.Context) {
	a := m.auth.Load()
	if a == nil {
		return
	}
	m.mu.Lock()
	live := make(map[uint64]string, len(m.leafs))
	for cid, key := range m.leafs {
		if m.srv.GetLeafNode(cid) == nil {
			delete(m.leafs, cid)
			continue
		}
		live[cid] = key
	}
	m.mu.Unlock()
	for cid, key := range live {
		_, _, err := a.check(ctx, key)
		if err == nil || !(errors.Is(err, errAppKeyInvalid) || errors.Is(err, errAppKeyExpired)) {
			continue
		}
		m.srv.Warnf("gateway: event stream mirror: leafnode %d closed: %v", cid, err)
		_ = m.srv.DisconnectClientByID(cid)
		m.mu.Lock()
		delete(m.leafs, cid)
		m.mu.Unlock()
	}
}

// blueStreams is green's JetStream view of blue (through the
// leafnode and blue's mappings).
func (m *clusterMirror) blueStreams() (jetstream.JetStream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blueJS != nil {
		return m.blueJS, nil
	}
	nc, err := nats.Connect("", nats.InProcessServer(m.srv), nats.CustomInboxPrefix(mirrorInboxPrefix), nats.Name("gwag-mirror"))
	if err != nil {
		return nil, err
	}
	js, err := jetstream.NewWithAPIPrefix(nc, mirrorAPIPrefix)
	if err != nil {
		nc.Close()
		return nil, err
	}
	m.blue, m.blueJS = nc, js
	return js, nil
}

// matchesFrom reports whether addr names one of green's configured
// sources (host and port).
func (m *clusterMirror) matchesFrom(addr string) bool {
	want, err := parseMirrorURLs(addr)
	if err != nil {
		return false
	}
	for _, w := range want {
		for _, u := range m.from {
			if strings.EqualFold(w.Host, u.Host) {
				return true
			}
		}
	}
	return false
}

// fromString is green's source for the admin view.
func (m *clusterMirror) fromString() string {
	parts := make([]string, len(m.from))
	for i, u := range m.from {
		parts[i] = u.String()
	}
	return strings.Join(parts, ",")
}

// bindMirror installs g's system keys as the mirror listener's
// credential (blue) and starts the recheck loop. Called from New.
func (g *Gateway) bindMirror() {
	cl := g.cfg.cluster
	if cl == nil || cl.mirror == nil || !cl.mirror.listen {
		return
	}
	m := cl.mirror
	m.setAuth(g, func(ctx context.Context, key string) (string, time.Time, error) {
		grant, err := g.checkSystemKey(ctx, key)
		return grant.id, grant.expires, err
	})
	go func() {
		t := time.NewTicker(mirrorRecheckInterval)
		defer t.Stop()
		defer m.clearAuth(g)
		for {
			select {
			case <-g.life.Done():
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(g.life, mirrorAuthTimeout)
				m.recheck(ctx)
				cancel()
			}
		}
	}()
}
