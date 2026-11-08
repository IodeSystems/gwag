package events

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// Serde turns a message into its wire body and back. Core calls it on
// every publish and every delivery, so any backend carries any wire
// format. The default, ProtoSerde, writes plain proto binary; the
// Gwag-* headers are set by Core either way.
//
// Stability: experimental
type Serde interface {
	// Encode sets out.Data for m. out already holds Subject, ID and the
	// Gwag-Type / Gwag-Channel headers; Encode may add headers. topic is
	// the backend's name for the channel (Topics.Topic, else
	// ch.Stream).
	Encode(ctx context.Context, ch Channel, topic string, m proto.Message, out *Message) error
	// Decode rewrites in.Data to plain proto binary of the body
	// message. When in has no Gwag-Type header (a record from a
	// producer that is not gwag) Decode sets it if the wire format
	// names the type. in is a copy; the backend keeps the original for
	// redelivery and dead letters.
	Decode(ctx context.Context, ch Channel, topic string, in *Message) error
}

// ProtoSerde is the default Serde: the body is proto binary, nothing
// else.
//
// Stability: experimental
type ProtoSerde struct{}

// Encode implements Serde.
func (ProtoSerde) Encode(_ context.Context, _ Channel, _ string, m proto.Message, out *Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	out.Data = b
	return nil
}

// Decode implements Serde. The body is already plain proto.
func (ProtoSerde) Decode(context.Context, Channel, string, *Message) error { return nil }

// Topics is implemented by a backend whose name for a channel is not
// ch.Stream (the Kafka backend's topic mapper). Core passes the name
// to the Serde, which may derive a schema subject from it.
//
// Stability: experimental
type Topics interface {
	Topic(ch Channel) string
}

// WithSerde sets the Serde. Default ProtoSerde.
//
// Stability: experimental
func WithSerde(s Serde) Option {
	return func(c *Core) { c.serde = s }
}
