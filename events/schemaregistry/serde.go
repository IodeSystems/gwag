package schemaregistry

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/events"
)

// HeaderSchemaID is set on a decoded message (not on the wire): the
// schema id the record was framed with.
//
// Stability: experimental
const HeaderSchemaID = "Gwag-Schema-Id"

// SubjectNameStrategy names the subject of a record's value schema.
//
// Stability: experimental
type SubjectNameStrategy func(topic string, md protoreflect.MessageDescriptor) string

// Confluent's subject name strategies.
//
// Stability: experimental
var (
	// TopicNameStrategy: "<topic>-value". Confluent's default; one
	// schema per topic.
	TopicNameStrategy SubjectNameStrategy = func(topic string, _ protoreflect.MessageDescriptor) string {
		return topic + "-value"
	}
	// RecordNameStrategy: the message full name. A topic may carry
	// several types.
	RecordNameStrategy SubjectNameStrategy = func(_ string, md protoreflect.MessageDescriptor) string {
		return string(md.FullName())
	}
	// TopicRecordNameStrategy: "<topic>-<message full name>".
	TopicRecordNameStrategy SubjectNameStrategy = func(topic string, md protoreflect.MessageDescriptor) string {
		return topic + "-" + string(md.FullName())
	}
)

// StrategyByName returns "topic", "record" or "topic-record".
//
// Stability: experimental
func StrategyByName(name string) (SubjectNameStrategy, error) {
	switch name {
	case "", "topic", "TopicNameStrategy":
		return TopicNameStrategy, nil
	case "record", "RecordNameStrategy":
		return RecordNameStrategy, nil
	case "topic-record", "TopicRecordNameStrategy":
		return TopicRecordNameStrategy, nil
	}
	return nil, fmt.Errorf("subject name strategy %q: want topic, record or topic-record", name)
}

type idMode int

const (
	modeLookup idMode = iota
	modeRegister
	modeLatest
	modeID
)

// Serde is an events.Serde with Confluent framing. Publish frames the
// body with the schema id of the message's file; delivery accepts a
// framed record from any producer and sets Gwag-Type from the schema
// id and message indexes when the record has no Gwag-Type header. A
// record whose first byte is not the magic byte is taken as plain
// proto (plain proto never starts with 0), so a topic can move to
// framing without a cut-over.
//
// Stability: experimental
type Serde struct {
	c         *Client
	strategy  SubjectNameStrategy
	mode      idMode
	modes     int // count of mode options given
	id        int
	normalize bool
	refSubj   func(string) string
	typeNames map[string]string
	channels  map[string]*Client // channel key -> its registry
	badChan   error

	ids sync.Map // channel key (when in channels) + "\x00" + subject + "\x00" + message full name -> encoding
}

type encoding struct {
	id      int
	indexes []int
}

// SerdeOption configures a Serde.
//
// Stability: experimental
type SerdeOption func(*Serde)

// WithSubjectNameStrategy sets the strategy. Default TopicNameStrategy.
//
// Stability: experimental
func WithSubjectNameStrategy(s SubjectNameStrategy) SerdeOption {
	return func(x *Serde) { x.strategy = s }
}

// AutoRegister registers the message's file (and its imports, as
// references) on first publish when the subject does not hold it. Use
// it for protos gwag owns. Without it (the default) publish only looks
// the schema up and fails when it is not registered.
//
// Stability: experimental
func AutoRegister() SerdeOption { return func(x *Serde) { x.mode, x.modes = modeRegister, x.modes+1 } }

// UseLatest frames with the subject's latest schema, without comparing
// it to the local proto (Confluent's use.latest.version). The message
// must exist in that schema under the same full name.
//
// Stability: experimental
func UseLatest() SerdeOption { return func(x *Serde) { x.mode, x.modes = modeLatest, x.modes+1 } }

// UseSchemaID frames every publish with schema id. The message must
// exist in that schema under the same full name.
//
// Stability: experimental
func UseSchemaID(id int) SerdeOption {
	return func(x *Serde) { x.mode, x.id, x.modes = modeID, id, x.modes+1 }
}

// Normalize passes normalize=true to lookups and registrations.
//
// Stability: experimental
func Normalize() SerdeOption { return func(x *Serde) { x.normalize = true } }

// WithReferenceSubject names the subject of an imported file. Default:
// the import path.
//
// Stability: experimental
func WithReferenceSubject(f func(path string) string) SerdeOption {
	return func(x *Serde) { x.refSubj = f }
}

// TypeNames maps a registry message full name to the local one, for a
// shadow copy in another package: a record framed as
// "corp.orders.v1.Order" is delivered with Gwag-Type
// "shop.v1.OrderShadow". Applied on decode only.
//
// Stability: experimental
func TypeNames(m map[string]string) SerdeOption {
	return func(x *Serde) { x.typeNames = m }
}

// ChannelClient sends the registry calls of channel key (its channel
// key, "corp.orders.v1.Order") to c instead of the serde's client, on
// publish and on delivery. For a channel another system produces: its
// schema ids are that system's, so its records decode only against its
// registry, or against the gwag context it was imported into
// (http://gwag:8080/schema-registry/contexts/.corp after `gwag ctl schema
// import --into .corp`). Repeatable, one per channel. An empty key or
// a nil client fails NewSerde.
//
// Stability: experimental
func ChannelClient(key string, c *Client) SerdeOption {
	return func(x *Serde) {
		if key == "" || c == nil {
			if x.badChan == nil {
				x.badChan = channelClientErr(key)
			}
			return
		}
		if x.channels == nil {
			x.channels = map[string]*Client{}
		}
		x.channels[key] = c
	}
}

// channelClientErr is the error of an empty channel key or a nil
// client. The Go, TypeScript and Python serdes reject it with this
// same text.
func channelClientErr(key string) error {
	return fmt.Errorf("schema registry: channel client for channel %q: want a channel key and a client", key)
}

// ErrExclusiveModes is returned by NewSerde when more than one of
// AutoRegister, UseLatest and UseSchemaID is given. The Go, TypeScript
// and Python serdes reject the combination with this same text.
//
// Stability: experimental
var ErrExclusiveModes = errors.New("schema registry: auto-register, use-latest and use-schema-id are mutually exclusive; set at most one")

// NewSerde returns a Serde over c. It returns ErrExclusiveModes when
// more than one of AutoRegister, UseLatest and UseSchemaID is given,
// and an error when a ChannelClient has an empty key or a nil client.
//
// Stability: experimental
func NewSerde(c *Client, opts ...SerdeOption) (*Serde, error) {
	s := &Serde{c: c, strategy: TopicNameStrategy}
	for _, o := range opts {
		o(s)
	}
	if s.modes > 1 {
		return nil, ErrExclusiveModes
	}
	if s.badChan != nil {
		return nil, s.badChan
	}
	return s, nil
}

// client is the registry of ch, and the cache prefix of its encodings.
func (s *Serde) client(ch events.Channel) (*Client, string) {
	if c, ok := s.channels[ch.Key]; ok {
		return c, ch.Key
	}
	return s.c, ""
}

// Subject returns the subject a publish of md to topic uses.
func (s *Serde) Subject(topic string, md protoreflect.MessageDescriptor) string {
	return s.strategy(topic, md)
}

// Encode implements events.Serde.
func (s *Serde) Encode(ctx context.Context, ch events.Channel, topic string, m proto.Message, out *events.Message) error {
	md := m.ProtoReflect().Descriptor()
	subject := s.strategy(topic, md)
	c, pfx := s.client(ch)
	enc, err := s.encoding(ctx, c, pfx, subject, md)
	if err != nil {
		return err
	}
	body, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	out.Data = Frame(enc.id, enc.indexes, body)
	return nil
}

func (s *Serde) encoding(ctx context.Context, c *Client, pfx, subject string, md protoreflect.MessageDescriptor) (encoding, error) {
	k := pfx + "\x00" + subject + "\x00" + string(md.FullName())
	if s.mode != modeLatest {
		if v, ok := s.ids.Load(k); ok {
			return v.(encoding), nil
		}
	}
	var enc encoding
	switch s.mode {
	case modeLookup, modeRegister:
		r, err := c.EnsureFile(ctx, subject, md.ParentFile(), RegisterOptions{
			Register: s.mode == modeRegister, Normalize: s.normalize, RefSubject: s.refSubj,
		})
		if err != nil {
			return encoding{}, err
		}
		enc = encoding{id: r.ID, indexes: MessageIndexes(md)}
	case modeLatest, modeID:
		id := s.id
		if s.mode == modeLatest {
			v, err := c.SubjectVersion(ctx, subject, "latest", "")
			if err != nil {
				return encoding{}, fmt.Errorf("latest of %s: %w", subject, err)
			}
			id = v.ID
		}
		fd, err := c.FileByID(ctx, id)
		if err != nil {
			return encoding{}, err
		}
		rmd := FindMessage(fd, md.FullName())
		if rmd == nil {
			return encoding{}, fmt.Errorf("schema id %d (subject %s) has no message %s", id, subject, md.FullName())
		}
		enc = encoding{id: id, indexes: MessageIndexes(rmd)}
	}
	if s.mode != modeLatest {
		s.ids.Store(k, enc)
	}
	return enc, nil
}

// Decode implements events.Serde.
func (s *Serde) Decode(ctx context.Context, ch events.Channel, _ string, in *events.Message) error {
	if len(in.Data) == 0 || in.Data[0] != MagicByte {
		return nil // plain proto
	}
	id, indexes, body, err := Unframe(in.Data)
	if err != nil {
		return err
	}
	c, _ := s.client(ch)
	fd, err := c.FileByID(ctx, id)
	if err != nil {
		return err
	}
	md, err := MessageAt(fd, indexes)
	if err != nil {
		return fmt.Errorf("schema id %d: %w", id, err)
	}
	name := string(md.FullName())
	if local, ok := s.typeNames[name]; ok {
		name = local
	}
	switch t := in.Headers[events.HeaderType]; {
	case t == "":
		in.Headers[events.HeaderType] = name
	case t != name && t != string(md.FullName()):
		return fmt.Errorf("%s header is %q but schema id %d frames %s", events.HeaderType, t, id, md.FullName())
	}
	in.Headers[HeaderSchemaID] = strconv.Itoa(id)
	in.Data = body
	return nil
}
