package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Event streams.
//
// In cluster mode the gateway keeps one JetStream stream per named
// channel: name eventStreamName(key) (EV_orders_v1_OrderEvents),
// subjects [the channel's pattern, named segments as `*`], file
// storage, limits retention, MaxAge WithEventStreamMaxAge (7 days by
// default), a 2-minute duplicates window (Nats-Msg-Id), and replicas
// from the cluster replica policy (WithClusterReplicas), raised and
// lowered with the KV buckets. The gateway creates or updates the
// stream when a slot declaring the channel registers; generated event
// code creates it only when it is missing (R1) and never updates it,
// so this config wins.
//
// Streams are never deleted by the gateway: when the declaring slot
// goes away the stored events stay until MaxAge. Delete a stream by
// hand (nats stream rm) to drop them.
//
// No cluster → no streams.

const (
	defaultEventStreamMaxAge = 7 * 24 * time.Hour
	eventStreamDuplicates    = 2 * time.Minute
	eventStreamCallTimeout   = 30 * time.Second
)

// WithEventStreamMaxAge sets how long the JetStream stream of a named
// event channel keeps a message (stream MaxAge). Default 7 days.
// Cluster mode only. Applied when the gateway next creates or updates
// the stream (slot registration, gateway start).
//
// Stability: experimental
func WithEventStreamMaxAge(d time.Duration) Option {
	return func(cfg *config) { cfg.eventStreamMaxAge = d }
}

// eventStreamState records the streams this node has ensured, by
// stream name → subject, so an index rebuild only touches new or
// changed channels.
type eventStreamState struct {
	mu    sync.Mutex
	known map[string]string
}

func (g *Gateway) eventStreamMaxAge() time.Duration {
	if d := g.cfg.eventStreamMaxAge; d > 0 {
		return d
	}
	return defaultEventStreamMaxAge
}

// eventStreamConfig is the stream config for one named channel.
func (g *Gateway) eventStreamConfig(name, subject string, replicas int) jetstream.StreamConfig {
	if replicas < 1 {
		replicas = 1
	}
	return jetstream.StreamConfig{
		Name:       name,
		Subjects:   []string{subject},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     g.eventStreamMaxAge(),
		Duplicates: eventStreamDuplicates,
		Replicas:   replicas,
	}
}

// ensureEventStreamsAsync starts a create-or-update for every channel
// whose stream this node has not ensured with the same subject yet.
// Caller holds g.mu (it reads g.peers); the JetStream calls run on
// their own goroutines.
func (g *Gateway) ensureEventStreamsAsync(chs []*eventChannel) {
	cl := g.cfg.cluster
	if cl == nil || cl.JS == nil || len(chs) == 0 {
		return
	}
	replicas := g.eventStreamReplicas()
	pt := g.peers
	st := &g.eventStreams
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.known == nil {
		st.known = map[string]string{}
	}
	for _, c := range chs {
		name := c.streamName()
		if st.known[name] == c.pattern {
			continue
		}
		st.known[name] = c.pattern
		go g.ensureEventStream(cl.JS, pt, name, c.pattern, replicas)
	}
}

// eventStreamReplicas is the replica count for a stream ensured now:
// the policy for the live peers, or the KV buckets' replica count
// (currentR) when that is higher. currentR alone lags behind a peer
// join: it changes only once the bucket raise is applied, and a
// stream ensured before that would get the old count. The raise
// (setEventStreamReplicas) only covers the streams this node had
// ensured when it started, so it would not correct that stream.
// Caller holds g.mu.
func (g *Gateway) eventStreamReplicas() int {
	replicas := 1
	if g.peers == nil {
		return replicas
	}
	if r := int(g.peers.currentR.Load()); r > replicas {
		replicas = r
	}
	if r := g.cfg.clusterReplicas.For(len(g.peers.LivePeers())); r > replicas {
		replicas = r
	}
	return replicas
}

// ensureEventStream creates or updates one stream (applyStream: no
// write when the stream already has the config). It never lowers the
// replica count of an existing stream (setEventStreamReplicas does,
// on ForgetPeer), nor below currentR when the stream is written: a
// bucket raise can finish after replicas was chosen. While a node is
// being evacuated it does not raise an existing stream, and a stream
// it creates is moved off that node (cluster_evacuate.go). A failure
// is logged and forgotten, so the next index rebuild retries.
func (g *Gateway) ensureEventStream(js jetstream.JetStream, pt *peerTracker, name, subject string, replicas int) {
	ctx, cancel := context.WithTimeout(g.life, eventStreamCallTimeout)
	defer cancel()
	var (
		evac    map[string]evacuatingPeer
		hold    bool
		created bool
	)
	if pt != nil {
		var err error
		evac, err = pt.evacuations(ctx)
		hold = err != nil || len(evac) > 0
	}
	_, err := applyStream(ctx, js, name, g.isJetStreamLeader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
		r := replicas
		if pt != nil {
			r = max(r, int(pt.currentR.Load()))
		}
		if cur != nil {
			if hold {
				r = cur.Replicas
			}
			r = max(r, cur.Replicas)
		}
		created = cur == nil
		return g.eventStreamConfig(name, subject, r), true
	})
	if err == nil {
		if created {
			g.moveOffEvacuating(ctx, js, name, evac)
		}
		return
	}
	st := &g.eventStreams
	st.mu.Lock()
	if st.known[name] == subject {
		delete(st.known, name)
	}
	st.mu.Unlock()
	if g.life.Err() != nil {
		return
	}
	if cl := g.cfg.cluster; cl != nil && cl.Server != nil {
		cl.Server.Warnf("gateway: event stream %s (subject %s): %v", name, subject, err)
	}
}

// setEventStreamReplicas sets the replica count of every stream this
// node ensured (every node ensures the stream of every registered
// channel). Called with the KV bucket update (peerTracker
// .setReplicas); lower and leader as there. A stream deleted by hand
// is skipped; the next ensure creates it. Returns the first error.
func (g *Gateway) setEventStreamReplicas(ctx context.Context, js jetstream.JetStream, r int, lower bool, leader func() bool) error {
	st := &g.eventStreams
	st.mu.Lock()
	names := make([]string, 0, len(st.known))
	subjects := make(map[string]string, len(st.known))
	for n, s := range st.known {
		names = append(names, n)
		subjects[n] = s
	}
	st.mu.Unlock()
	sort.Strings(names)
	var firstErr error
	for _, n := range names {
		_, err := applyStream(ctx, js, n, leader, func(cur *jetstream.StreamConfig) (jetstream.StreamConfig, bool) {
			if cur == nil {
				return jetstream.StreamConfig{}, false
			}
			return g.eventStreamConfig(n, subjects[n], nextReplicas(cur.Replicas, r, lower)), true
		})
		if err != nil && !errors.Is(err, errStreamMissing) && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", n, err)
		}
	}
	return firstErr
}
