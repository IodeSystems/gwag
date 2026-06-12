package relay

import "github.com/nats-io/nats.go"

// Source is the event origin the relay fans out from. Subscribe delivers each
// raw payload published on channel to deliver until cancel is called. deliver
// runs on the Source's own goroutine and must not block — the relay's callback
// only enqueues onto a bounded per-connection buffer.
type Source interface {
	Subscribe(channel string, deliver func(payload []byte)) (cancel func(), err error)
}

// SourceFunc adapts a bare subscribe function to a Source.
type SourceFunc func(channel string, deliver func(payload []byte)) (cancel func(), err error)

// Subscribe implements Source.
func (f SourceFunc) Subscribe(channel string, deliver func(payload []byte)) (func(), error) {
	return f(channel, deliver)
}

// NatsSource adapts a NATS connection to a Source: each subscription is a core
// NATS subscription on the channel subject (at-most-once — keep handlers
// idempotent; missed events during a disconnect are covered by the client's
// refetch), delivering message bytes verbatim.
func NatsSource(nc *nats.Conn) Source {
	return SourceFunc(func(channel string, deliver func([]byte)) (func(), error) {
		sub, err := nc.Subscribe(channel, func(m *nats.Msg) { deliver(m.Data) })
		if err != nil {
			return func() {}, err
		}
		return func() { _ = sub.Unsubscribe() }, nil
	})
}
