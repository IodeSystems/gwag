package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	natsd "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		// A group being moved or resized can still change members;
		// Replicas lists the current ones only.
		if ci.Desired != nil {
			why = append(why, "moving")
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

// streamOfEntry is the stream name of a "STREAM: reason" entry.
func streamOfEntry(e string) string {
	name, _, _ := strings.Cut(e, ": ")
	return name
}

// replicaTarget is the replica count the policy gives for the live
// peers, as reconcileReplicas raises to.
func (t *peerTracker) replicaTarget() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gw.cfg.clusterReplicas.For(len(t.live))
}

// streamsBelowTarget lists every stream the gateway manages (its KV
// buckets and named-channel event streams) whose replica group is not
// whole at target replicas: configured below target, short of its
// configured count, without a leader, being moved, or with a replica
// that is not current. Each entry is "STREAM: reason". Streams the
// gateway does not manage are left out: it does not raise them.
func (t *peerTracker) streamsBelowTarget(ctx context.Context, target int) ([]string, error) {
	managed := map[string]bool{}
	for _, b := range t.buckets {
		managed["KV_"+b] = true
	}
	st := &t.gw.eventStreams
	st.mu.Lock()
	for n := range st.known {
		managed[n] = true
	}
	st.mu.Unlock()
	var out []string
	lister := t.js.ListStreams(ctx)
	for info := range lister.Info() {
		if !managed[info.Config.Name] {
			continue
		}
		var why []string
		if r := info.Config.Replicas; r < target {
			why = append(why, fmt.Sprintf("R%d, cluster target R%d", r, target))
		}
		if ci := info.Cluster; ci != nil {
			placed := len(ci.Replicas)
			if ci.Leader == "" {
				why = append(why, "no leader")
			} else {
				placed++
			}
			if ci.Desired != nil {
				why = append(why, "moving")
			}
			if placed < info.Config.Replicas {
				why = append(why, fmt.Sprintf("%d of %d replicas placed", placed, info.Config.Replicas))
			}
			for _, r := range ci.Replicas {
				if !r.Current {
					why = append(why, r.Name+" not current")
				}
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

// evacuateNotReadyError: EvacuatePeer did not start the evacuation
// of Peer because Streams are not whole at Target replicas. The
// control plane answers FailedPrecondition (admin API 409).
type evacuateNotReadyError struct {
	Peer    string
	Target  int
	Streams []string
}

func (e *evacuateNotReadyError) Error() string {
	return fmt.Sprintf("cluster: evacuation of %s not started: %d stream(s) not at R%d with every replica current, and an evacuation holds raises back until the node is forgotten; try again once they are (gwag ctl peer evacuate --wait does): %s",
		e.Peer, len(e.Streams), e.Target, strings.Join(e.Streams, "; "))
}

// status is the FailedPrecondition answer, with one
// PreconditionFailure violation per stream (subject: the stream,
// description: why it is not whole).
func (e *evacuateNotReadyError) status() error {
	st := status.New(codes.FailedPrecondition, e.Error())
	pf := &errdetails.PreconditionFailure{}
	for _, s := range e.Streams {
		name, why, _ := strings.Cut(s, ": ")
		pf.Violations = append(pf.Violations, &errdetails.PreconditionFailure_Violation{
			Type: evacuateViolationType, Subject: name, Description: why,
		})
	}
	if withDetails, err := st.WithDetails(pf); err == nil {
		st = withDetails
	}
	return st.Err()
}

// evacuateViolationType is the PreconditionFailure violation type of
// a stream that keeps an evacuation from starting.
const evacuateViolationType = "STREAM_NOT_WHOLE"

// evacuatePeer implements the EvacuatePeer RPC.
func (g *Gateway) evacuatePeer(ctx context.Context, nodeID string, checkOnly, leave bool) (name string, remaining []string, done, left bool, err error) {
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t == nil {
		return "", nil, false, false, errors.New("cluster: not configured or not started")
	}
	// Asked of the stream leader, as ForgetPeer does: a direct get can
	// be answered by a replica that is behind.
	kctx, cancel := kvCtx(ctx)
	v, ok, err := kvLeaderGet(kctx, g.cfg.cluster.Conn, peersBucketName, nodeID)
	cancel()
	if err != nil {
		return "", nil, false, false, fmt.Errorf("cluster: peer lookup: %w", err)
	}
	if !ok {
		return "", nil, false, false, fmt.Errorf("cluster: peer %s is not live; a stopped peer is removed with ForgetPeer", nodeID)
	}
	var pe peerEntry
	if err := json.Unmarshal(v, &pe); err != nil || pe.Name == "" {
		return "", nil, false, false, fmt.Errorf("cluster: peer %s has no server name", nodeID)
	}
	name = pe.Name
	// An evacuation holds every replica raise back until the node is
	// forgotten. One that starts with a stream below the cluster's
	// replica count, or with a replica that is not current, leaves
	// that stream short until then: an R1 stream on the next node to
	// go has a single copy. The first call is refused until every
	// stream is whole; a call that renews a recorded evacuation is not
	// checked again (its moves make streams not current for a while).
	kctx, cancel = kvCtx(ctx)
	evac, err := t.evacuations(kctx)
	cancel()
	if err != nil {
		return name, nil, false, false, fmt.Errorf("cluster: read evacuation record: %w", err)
	}
	var short []string
	if _, started := evac[nodeID]; !started {
		target := t.replicaTarget()
		if short, err = t.streamsBelowTarget(ctx, target); err != nil {
			return name, nil, false, false, fmt.Errorf("cluster: list streams: %w", err)
		}
		if len(short) > 0 && !checkOnly {
			// Raise what is short now, if no other evacuation holds
			// raises back; the caller tries again.
			t.kickReconcile(g.life)
			return name, nil, false, false, &evacuateNotReadyError{Peer: name, Target: target, Streams: short}
		}
	}
	if !checkOnly {
		// Recorded first, so no raise starts after the evacuation.
		kctx, cancel := kvCtx(ctx)
		err := t.markEvacuating(kctx, nodeID, name)
		cancel()
		if err != nil {
			return name, nil, false, false, fmt.Errorf("cluster: record evacuation: %w", err)
		}
		if err := g.jsServerAPI(ctx, jsAPIServerEvacuate, name, false); err != nil {
			return name, nil, false, false, err
		}
	}
	remaining, err = streamsOnPeer(ctx, t.js, name)
	if err != nil {
		return name, nil, false, false, fmt.Errorf("cluster: list streams: %w", err)
	}
	// A check before the evacuation starts reports what would refuse
	// it too, one entry per stream.
	if len(short) > 0 {
		listed := map[string]bool{}
		for _, s := range short {
			listed[streamOfEntry(s)] = true
		}
		for _, r := range remaining {
			if !listed[streamOfEntry(r)] {
				short = append(short, r)
			}
		}
		sort.Strings(short)
		remaining = short
	}
	done = len(remaining) == 0
	if leave && done {
		if err := g.jsServerAPI(ctx, jsAPIServerRemove, name, true); err != nil {
			return name, remaining, done, false, err
		}
		left = true
		// Out of the meta group: nothing can be placed on it. Best
		// effort; the record expires otherwise.
		kctx, cancel := kvCtx(ctx)
		_ = t.clearEvacuating(kctx, nodeID)
		cancel()
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
	entries, err := readPeerEntries(kctx, t.gw.cfg.cluster.Conn)
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

// Evacuation records.
//
// NATS has no placement option that keeps a server out of a new
// stream group: placement picks among the online meta members (tags
// are server config, fixed at start). $JS.API.SERVER.EVACUATE keeps
// the server out only of the groups it was a member of, and only
// until each is back at full size. A replica raise or a stream create
// during an evacuation can therefore place a replica on the node
// again.
//
// evacuatePeer records the node under clusterEvacuationsKey in the
// gwag-cluster bucket, with an expiry that every evacuate call renews.
// While a record is active:
//
//   - no node raises the replica count of a bucket or stream
//     (reconcileReplicas, ensureEventStream);
//   - a node that creates a bucket or stream sends the evacuation
//     again for each recorded node holding a replica of it
//     (moveOffEvacuating), so the new group moves off the node and
//     keeps it excluded.
//
// The record goes when the node leaves the meta group (ForgetPeer,
// evacuate with leave) or when it expires: an evacuation whose caller
// crashed or gave up blocks raises for evacuationTTL at most.
// Gateways older than this (v2.11.0) do not read the record.

const (
	clusterEvacuationsKey = "evacuations"
	evacuationTTL         = 5 * time.Minute
)

// evacuationRecord is the value of clusterEvacuationsKey: every node
// being evacuated, by node id.
type evacuationRecord struct {
	Peers map[string]evacuatingPeer `json:"peers"`
}

type evacuatingPeer struct {
	Name        string `json:"name"`
	UntilUnixMs int64  `json:"until_unix_ms"`
	SetBy       string `json:"set_by,omitempty"`
}

// getEvacuations reads the record with expired entries removed, and
// its revision (0 when the key has no message). It asks the stream
// leader (kvLeaderEntry): a direct get can be answered by a replica
// that is behind, which shows a record that was cleared, misses one
// that was written, or gives an old revision that every compare and
// set then loses.
func getEvacuations(ctx context.Context, nc *nats.Conn) (evacuationRecord, uint64, error) {
	rec := evacuationRecord{Peers: map[string]evacuatingPeer{}}
	v, rev, ok, err := kvLeaderEntry(ctx, nc, clusterBucketName, clusterEvacuationsKey)
	if err != nil {
		return rec, 0, fmt.Errorf("evacuations: %w", err)
	}
	if !ok {
		return rec, rev, nil
	}
	if err := json.Unmarshal(v, &rec); err != nil {
		return rec, 0, fmt.Errorf("evacuations: %w", err)
	}
	if rec.Peers == nil {
		rec.Peers = map[string]evacuatingPeer{}
	}
	now := time.Now().UnixMilli()
	for id, p := range rec.Peers {
		if p.UntilUnixMs <= now {
			delete(rec.Peers, id)
		}
	}
	return rec, rev, nil
}

// updateEvacuations applies change to the record in kv (the
// gwag-cluster bucket) with compare and set, reading again when
// another node wrote it in between.
func updateEvacuations(ctx context.Context, nc *nats.Conn, kv jetstream.KeyValue, change func(map[string]evacuatingPeer)) error {
	for attempt := 0; ; attempt++ {
		rec, rev, err := getEvacuations(ctx, nc)
		if err != nil {
			return err
		}
		change(rec.Peers)
		b, _ := json.Marshal(rec)
		if rev == 0 {
			_, err = kv.Create(ctx, clusterEvacuationsKey, b)
		} else {
			_, err = kv.Update(ctx, clusterEvacuationsKey, b, rev)
		}
		if err == nil {
			return nil
		}
		// A lost compare and set is an API error (key exists, wrong
		// last sequence); anything else is final.
		var apiErr *jetstream.APIError
		if attempt >= 10 || ctx.Err() != nil || !(errors.As(err, &apiErr) || errors.Is(err, jetstream.ErrKeyExists)) {
			return fmt.Errorf("evacuations: %w", err)
		}
	}
}

// markEvacuating records (or renews) the evacuation of nodeID.
func (t *peerTracker) markEvacuating(ctx context.Context, nodeID, name string) error {
	until := time.Now().Add(evacuationTTL).UnixMilli()
	by := t.gw.cfg.cluster.Server.Name()
	return updateEvacuations(ctx, t.gw.cfg.cluster.Conn, t.cluster, func(m map[string]evacuatingPeer) {
		m[nodeID] = evacuatingPeer{Name: name, UntilUnixMs: until, SetBy: by}
	})
}

// clearEvacuating drops the record of nodeID, if any.
func (t *peerTracker) clearEvacuating(ctx context.Context, nodeID string) error {
	rec, _, err := getEvacuations(ctx, t.gw.cfg.cluster.Conn)
	if err != nil {
		return err
	}
	if _, ok := rec.Peers[nodeID]; !ok {
		return nil
	}
	return updateEvacuations(ctx, t.gw.cfg.cluster.Conn, t.cluster, func(m map[string]evacuatingPeer) { delete(m, nodeID) })
}

// evacuations returns the active records. A read error is returned
// with a nil map; callers that raise treat it as "evacuating".
func (t *peerTracker) evacuations(ctx context.Context) (map[string]evacuatingPeer, error) {
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	rec, _, err := getEvacuations(kctx, t.gw.cfg.cluster.Conn)
	if err != nil {
		return nil, err
	}
	return rec.Peers, nil
}

// evacuating reports whether a node is being evacuated. A read error
// counts as yes: the caller skips a raise, and the next tick retries.
func (t *peerTracker) evacuating(ctx context.Context) bool {
	m, err := t.evacuations(ctx)
	return err != nil || len(m) > 0
}

// moveOffEvacuating sends $JS.API.SERVER.EVACUATE again for every
// recorded node that holds a replica of stream name. Called after a
// create; evac is the active record (empty: nothing to do). Best
// effort: the operator's evacuate loop checks every stream anyway.
func (g *Gateway) moveOffEvacuating(ctx context.Context, js jetstream.JetStream, name string, evac map[string]evacuatingPeer) {
	if len(evac) == 0 {
		return
	}
	nodes, err := streamNodes(ctx, js, name)
	if err != nil {
		return
	}
	for _, p := range evac {
		if !slices.Contains(nodes, p.Name) {
			continue
		}
		err := g.jsServerAPI(ctx, jsAPIServerEvacuate, p.Name, true)
		if cl := g.cfg.cluster; cl != nil && cl.Server != nil && err != nil {
			cl.Server.Warnf("gateway: %s was placed on %s, which is being evacuated; evacuate again: %v", name, p.Name, err)
		}
	}
}

// streamNodes returns the servers holding, or about to hold, a
// replica of stream name.
func streamNodes(ctx context.Context, js jetstream.JetStream, name string) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, applyStreamAttemptTimeout)
	defer cancel()
	s, err := js.Stream(c, name)
	if err != nil {
		return nil, err
	}
	ci := s.CachedInfo().Cluster
	if ci == nil {
		return nil, nil
	}
	var out []string
	if ci.Leader != "" {
		out = append(out, ci.Leader)
	}
	for _, r := range ci.Replicas {
		out = append(out, r.Name)
	}
	if ci.Desired != nil {
		for _, r := range ci.Desired.Replicas {
			out = append(out, r.Name)
		}
	}
	return out, nil
}
