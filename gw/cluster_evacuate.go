package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStream server APIs used to move a node out of the cluster.
// Both answer only in the system account, from the meta leader
// (Cluster.sys is the connection for them):
//
//   - $JS.API.SERVER.EVACUATE {"peer": name}: every stream with a
//     replica on the server migrates to another server; the server
//     stays out of those groups until they are back at full size.
//     Evacuate one server at a time and remove it from the meta group
//     before the next: once a group is whole again an evacuated server
//     can be picked as a replacement for the next one.
//   - $JS.API.SERVER.REMOVE {"peer": name}: the server leaves the meta
//     group. Stream replicas still on it are dropped and replaced. A
//     live server that is removed turns its JetStream off.
const (
	jsAPIServerEvacuate = "$JS.API.SERVER.EVACUATE"
	jsAPIServerRemove   = "$JS.API.SERVER.REMOVE"

	// errCodeServerNotMember is JSClusterServerNotMemberErr: the
	// server is not (or no longer) in the meta group.
	errCodeServerNotMember = 10044
)

type jsServerRequest struct {
	Peer string `json:"peer"`
}

type jsServerResponse struct {
	Error *struct {
		Code        int    `json:"code"`
		ErrCode     int    `json:"err_code"`
		Description string `json:"description"`
	} `json:"error,omitempty"`
	Success bool `json:"success"`
}

// jsServerAPI sends a system-account JetStream server request for
// server name and retries while the meta leader does not answer (an
// election in progress). notMemberOK turns "not a member" into
// success.
func (g *Gateway) jsServerAPI(ctx context.Context, subject, name string, notMemberOK bool) error {
	cl := g.cfg.cluster
	if cl == nil || cl.sys == nil {
		return errors.New("cluster: no system account connection (standalone node)")
	}
	body, _ := json.Marshal(jsServerRequest{Peer: name})
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if ctx.Err() != nil {
			break
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		msg, err := cl.sys.RequestWithContext(rctx, subject, body)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		var resp jsServerResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			return fmt.Errorf("%s: %w", subject, err)
		}
		if resp.Error != nil {
			if notMemberOK && resp.Error.ErrCode == errCodeServerNotMember {
				return nil
			}
			return fmt.Errorf("%s %s: %s (err_code %d)", subject, name, resp.Error.Description, resp.Error.ErrCode)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return fmt.Errorf("%s %s: %w", subject, name, lastErr)
}

// streamsOnPeer lists every stream of the gateway's account that
// has a replica on server name, or a replica that is not current.
// Each entry is "STREAM: reason".
func streamsOnPeer(ctx context.Context, js jetstream.JetStream, name string) ([]string, error) {
	var out []string
	lister := js.ListStreams(ctx)
	for info := range lister.Info() {
		ci := info.Cluster
		if ci == nil {
			continue
		}
		var why []string
		if ci.Leader == name {
			why = append(why, "leader on peer")
		}
		if ci.Leader == "" {
			why = append(why, "no leader")
		}
		for _, r := range ci.Replicas {
			switch {
			case r.Name == name:
				why = append(why, "replica on peer")
			case !r.Current:
				why = append(why, r.Name+" not current")
			}
		}
		if len(why) > 0 {
			out = append(out, info.Config.Name+": "+strings.Join(why, ", "))
		}
	}
	if err := lister.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// evacuatePeer implements the EvacuatePeer RPC.
func (g *Gateway) evacuatePeer(ctx context.Context, nodeID string, checkOnly, leave bool) (name string, remaining []string, done, left bool, err error) {
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t == nil {
		return "", nil, false, false, errors.New("cluster: not configured or not started")
	}
	kctx, cancel := kvCtx(ctx)
	e, err := t.peers.Get(kctx, nodeID)
	cancel()
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return "", nil, false, false, fmt.Errorf("cluster: peer %s is not live; a stopped peer is removed with ForgetPeer", nodeID)
		}
		return "", nil, false, false, fmt.Errorf("cluster: peer lookup: %w", err)
	}
	var pe peerEntry
	if err := json.Unmarshal(e.Value(), &pe); err != nil || pe.Name == "" {
		return "", nil, false, false, fmt.Errorf("cluster: peer %s has no server name", nodeID)
	}
	name = pe.Name
	if !checkOnly {
		if err := g.jsServerAPI(ctx, jsAPIServerEvacuate, name, false); err != nil {
			return name, nil, false, false, err
		}
	}
	remaining, err = streamsOnPeer(ctx, t.js, name)
	if err != nil {
		return name, nil, false, false, fmt.Errorf("cluster: list streams: %w", err)
	}
	done = len(remaining) == 0
	if leave && done {
		if err := g.jsServerAPI(ctx, jsAPIServerRemove, name, true); err != nil {
			return name, remaining, done, false, err
		}
		left = true
	}
	return name, remaining, done, left, nil
}

// removeMetaPeer removes a peer that has expired from the JetStream
// meta group. Skipped (false) when this node never saw the peer's
// name, or a live peer carries the same name: the meta group knows
// servers by name, so that would remove the live one (a node restarted
// with the same --node-name gets a new node id).
func (t *peerTracker) removeMetaPeer(ctx context.Context, nodeID string) (bool, error) {
	if cl := t.gw.cfg.cluster; cl == nil || cl.sys == nil {
		return false, nil // standalone JetStream: no meta group
	}
	t.mu.Lock()
	name := t.names[nodeID]
	t.mu.Unlock()
	if name == "" {
		return false, nil
	}
	kctx, cancel := kvCtx(ctx)
	entries, err := readPeerEntries(kctx, t.peers)
	cancel()
	if err != nil {
		return false, err
	}
	for _, pe := range entries {
		if pe.Name == name && pe.NodeID != nodeID {
			return false, nil
		}
	}
	if err := t.gw.jsServerAPI(ctx, jsAPIServerRemove, name, true); err != nil {
		return false, err
	}
	return true, nil
}

// jetStreamMeta is the meta group as this node sees it.
type jetStreamMeta struct {
	Leader  string
	Size    int
	Offline []string
}

func (g *Gateway) jetStreamMeta() jetStreamMeta {
	cl := g.cfg.cluster
	if cl == nil || cl.Server == nil {
		return jetStreamMeta{}
	}
	jsz, err := cl.Server.Jsz(&natsd.JSzOptions{})
	if err != nil || jsz == nil || jsz.Meta == nil {
		return jetStreamMeta{}
	}
	m := jetStreamMeta{Leader: jsz.Meta.Leader, Size: jsz.Meta.Size}
	for _, r := range jsz.Meta.Replicas {
		if r.Offline {
			m.Offline = append(m.Offline, r.Name)
		}
	}
	sort.Strings(m.Offline)
	return m
}
