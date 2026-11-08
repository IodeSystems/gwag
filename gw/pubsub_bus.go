package gateway

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/gw/gat"
	"github.com/iodesystems/gwag/v2/gw/gat/gatnats"
)

// ps.pub / ps.sub transport modes, as reported by HealthHandler.
const (
	pubSubModeLocal    = "local"          // no cluster: single node
	pubSubModeNATS     = "nats"           // cluster, NATS connected
	pubSubModeDegraded = "local_degraded" // cluster, NATS down
)

// initPubSubBus picks the ps transport: a LocalBus without a cluster,
// a failoverBus over the cluster connection with one. Called once from
// New.
func (g *Gateway) initPubSubBus() {
	if g.cfg.cluster == nil {
		g.psBus = gat.NewLocalBus()
		return
	}
	g.psFailover = newFailoverBus(g.cfg.cluster.Conn, g.onPubSubBusChange)
	g.psBus = g.psFailover
}

// onPubSubBusChange logs a degrade / recover transition and drives
// the pubsub_bus_degraded gauge.
func (g *Gateway) onPubSubBusChange(degraded bool) {
	if pm := unwrapPrometheusMetrics(g.cfg.metrics); pm != nil {
		pm.setPubSubBusDegraded(degraded)
	}
	cl := g.cfg.cluster
	if cl == nil || cl.Server == nil {
		return
	}
	if degraded {
		cl.Server.Warnf("gateway: pubsub: NATS connection down; ps.pub/ps.sub on this node are local-only until it reconnects")
	} else {
		cl.Server.Noticef("gateway: pubsub: NATS connection back; ps.pub/ps.sub on NATS again")
	}
}

func (g *Gateway) pubSubBusMode() string {
	switch {
	case g.psFailover == nil:
		return pubSubModeLocal
	case g.psFailover.Degraded():
		return pubSubModeDegraded
	default:
		return pubSubModeNATS
	}
}

// failoverBus is the ps.pub / ps.sub transport in cluster mode: NATS
// while the connection is up, this node's LocalBus while it is down.
//
// Every subscription is opened on both buses. Each publish goes to
// exactly one of them, so a subscriber receives an event once:
//
//   - healthy: publish to NATS only. Local subscribers get the event
//     back through NATS echo; the local bus sees no publishes.
//   - degraded: publish to the local bus only. Other nodes miss it.
//
// A publish that nats.go accepted just before the disconnect was
// noticed sits in nats.go's reconnect buffer and is delivered through
// NATS after reconnect — still once. nats.go re-sends every open
// subscription on reconnect; subscriptions whose NATS side failed
// (connection closed) are retried when the connection is back, before
// publishes switch back to NATS.
//
// The degraded flag follows nc.StatusChanged. Events published while
// degraded reach this node's subscribers only, and nothing is
// replayed to other nodes after reconnect.
type failoverBus struct {
	nc       *nats.Conn    // nil: cluster configured without a connection
	primary  gat.HeaderBus // gatnats over nc; nil when nc is nil
	local    *gat.LocalBus
	degraded atomic.Bool
	onChange func(degraded bool)

	mu     sync.Mutex
	subs   map[uint64]*failoverSub
	nextID uint64

	status chan nats.Status
	done   chan struct{}
}

type failoverSub struct {
	pattern      string
	fn           func(channel string, header gat.Header, payload []byte)
	localUnsub   func()
	primaryUnsub func() // nil until the NATS subscribe succeeds
}

var _ gat.HeaderBus = (*failoverBus)(nil)

// newFailoverBus starts degraded when nc is nil or not connected and
// reports that through onChange before returning. onChange runs on the
// watcher goroutine for later transitions.
func newFailoverBus(nc *nats.Conn, onChange func(degraded bool)) *failoverBus {
	b := &failoverBus{
		nc:       nc,
		local:    gat.NewLocalBus(),
		onChange: onChange,
		subs:     map[uint64]*failoverSub{},
		done:     make(chan struct{}),
	}
	if nc == nil {
		b.degraded.Store(true)
		close(b.done)
		b.notify(true)
		return b
	}
	b.primary = gatnats.New(nc)
	// Register before reading the status so no transition is missed.
	b.status = nc.StatusChanged()
	if natsDegraded(nc.Status()) {
		b.degraded.Store(true)
		b.notify(true)
	}
	go b.watch()
	return b
}

// natsDegraded reports whether ps traffic should stay local for
// connection status st. Draining (shutdown) still accepts publishes.
func natsDegraded(st nats.Status) bool {
	switch st {
	case nats.CONNECTED, nats.DRAINING_SUBS, nats.DRAINING_PUBS:
		return false
	}
	return true
}

func (b *failoverBus) notify(degraded bool) {
	if b.onChange != nil {
		b.onChange(degraded)
	}
}

func (b *failoverBus) watch() {
	defer close(b.done)
	for range b.status {
		b.refresh()
	}
}

// refresh re-reads the connection status (status events can be
// dropped when the listener buffer is full, so the event value itself
// is not trusted) and applies a transition.
func (b *failoverBus) refresh() {
	deg := natsDegraded(b.nc.Status())
	b.mu.Lock()
	if deg == b.degraded.Load() {
		b.mu.Unlock()
		return
	}
	if !deg {
		// Back on NATS: bind any subscription that has no NATS side
		// before publishes stop going to the local bus.
		for _, s := range b.subs {
			if s.primaryUnsub == nil {
				if un, err := b.primary.SubscribeMsg(s.pattern, s.fn); err == nil {
					s.primaryUnsub = un
				}
			}
		}
	}
	b.degraded.Store(deg)
	b.mu.Unlock()
	b.notify(deg)
}

// Degraded reports whether ps traffic is local-only on this node.
func (b *failoverBus) Degraded() bool { return b.degraded.Load() }

// Publish sends to NATS when healthy, else to the local bus. A NATS
// publish refused because the connection is gone (closed, draining,
// reconnect buffer full) was not sent, so it falls back to local
// delivery without risking a duplicate. Other NATS errors (e.g.
// payload too large) are returned as before.
func (b *failoverBus) Publish(channel string, payload []byte) error {
	return b.PublishMsg(channel, nil, payload)
}

// PublishMsg is Publish with headers; the same failover rule applies.
func (b *failoverBus) PublishMsg(channel string, header gat.Header, payload []byte) error {
	if b.primary != nil && !b.degraded.Load() {
		err := b.primary.PublishMsg(channel, header, payload)
		if err == nil || !connectionGone(err) {
			return err
		}
	}
	return b.local.PublishMsg(channel, header, payload)
}

func connectionGone(err error) bool {
	return errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionDraining) ||
		errors.Is(err, nats.ErrReconnectBufExceeded)
}

// Subscribe opens pattern on the local bus and on NATS. While healthy
// a NATS error is returned (as before the bus existed); while degraded
// the subscription is kept local-only and NATS is retried on
// reconnect.
func (b *failoverBus) Subscribe(pattern string, fn func(channel string, payload []byte)) (func(), error) {
	return b.SubscribeMsg(pattern, func(channel string, _ gat.Header, payload []byte) {
		fn(channel, payload)
	})
}

// SubscribeMsg is Subscribe with headers.
func (b *failoverBus) SubscribeMsg(pattern string, fn func(channel string, header gat.Header, payload []byte)) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lu, err := b.local.SubscribeMsg(pattern, fn)
	if err != nil {
		return nil, err
	}
	s := &failoverSub{pattern: pattern, fn: fn, localUnsub: lu}
	if b.primary != nil {
		pu, err := b.primary.SubscribeMsg(pattern, fn)
		switch {
		case err == nil:
			s.primaryUnsub = pu
		case !b.degraded.Load():
			lu()
			return nil, err
		}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = s
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			pu := s.primaryUnsub
			b.mu.Unlock()
			s.localUnsub()
			if pu != nil {
				pu()
			}
		})
	}, nil
}

// Close stops the status watcher. It does not close nc.
func (b *failoverBus) Close() {
	if b.nc != nil && b.status != nil {
		b.nc.RemoveStatusListener(b.status)
	}
	<-b.done
}
