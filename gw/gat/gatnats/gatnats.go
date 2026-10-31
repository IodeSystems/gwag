// Package gatnats is a gat.Bus over a NATS connection. It is a
// separate package so gat itself does not depend on nats.go.
//
// A gat channel is the NATS subject and the payload is the message
// body, unchanged; gwag uses the same mapping for ps.pub / ps.sub, so
// gat and gwag nodes on one NATS cluster see each other's events.
//
//	nc, _ := nats.Connect(url)
//	g.UseBus(gatnats.New(nc))
//
// Stability: experimental
package gatnats

import (
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Bus publishes and subscribes on a NATS connection. Delivery follows
// NATS core semantics: at most once, and a subscription's fn runs on
// that subscription's NATS goroutine. The caller owns nc; Bus never
// closes it.
//
// Stability: experimental
type Bus struct {
	nc *nats.Conn
}

var _ gat.Bus = (*Bus)(nil)

// New returns a Bus over nc.
//
// Stability: experimental
func New(nc *nats.Conn) *Bus {
	return &Bus{nc: nc}
}

// Publish sends payload on subject channel. While nc is reconnecting,
// nats.go buffers the message (up to its reconnect buffer) and sends
// it after reconnect.
//
// Stability: experimental
func (b *Bus) Publish(channel string, payload []byte) error {
	return b.nc.Publish(channel, payload)
}

// Subscribe opens a NATS subscription on pattern. nats.go re-sends
// the subscription to the server after a reconnect.
//
// Stability: experimental
func (b *Bus) Subscribe(pattern string, fn func(channel string, payload []byte)) (func(), error) {
	sub, err := b.nc.Subscribe(pattern, func(m *nats.Msg) {
		fn(m.Subject, m.Data)
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = sub.Unsubscribe() })
	}, nil
}
