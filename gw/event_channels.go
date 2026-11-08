package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/gat"
	psv1 "github.com/iodesystems/gwag/v2/gw/proto/ps/v1"
)

// Typed event channels.
//
// A named binding (`(gwag.ps.v1.binding) = { name, pattern }`)
// declares the channel "<proto package>.<name>". A message on a named
// channel is binary proto (proto.Marshal of the message, not the
// gwag.ps.v1.Event envelope) with three NATS headers:
//
//	Gwag-Type    message full name, e.g. orders.v1.OrderCreated
//	Gwag-Channel channel key, e.g. orders.v1.OrderEvents
//	Nats-Msg-Id  message id; JetStream drops a repeat within the
//	             stream's duplicates window
//
// ps.sub transcodes such a message to an Event whose payload is the
// protojson of the message. ps.pub on a named channel does the
// reverse. Unnamed channels keep the Event envelope.

// Header names of a typed event message.
//
// Stability: experimental
const (
	EventHeaderType    = "Gwag-Type"
	EventHeaderChannel = "Gwag-Channel"
	EventHeaderMsgID   = "Nats-Msg-Id"
)

var psEventFullName = psv1.File_gw_proto_ps_v1_ps_proto.Messages().ByName("Event").FullName()

// decodeTypedEvent turns a typed event message into the ps.sub Event
// payload: payload = protojson of the message, payloadType = its
// Gwag-Type. A type no registered slot declares, or a body that is
// not that type, is dropped with a rate-limited log line and a
// pubsub_typed_skipped_total count. Runs on the bus goroutine.
func (g *Gateway) decodeTypedEvent(channel string, header gat.Header, data []byte, outputDesc protoreflect.MessageDescriptor) (map[string]any, bool) {
	if outputDesc.FullName() != psEventFullName {
		return nil, false // typed events only ride ps channels
	}
	fqn := header.Get(EventHeaderType)
	desc := g.channelBindingIndex.Load().messageDesc(fqn)
	if desc == nil {
		g.typedEventSkip("unknown_type", "ps.sub: channel %s: dropped message of unknown type %q (no registered proto declares it)", channel, fqn)
		return nil, false
	}
	msg := dynamicpb.NewMessage(desc)
	if err := proto.Unmarshal(data, msg); err != nil {
		g.typedEventSkip("decode", "ps.sub: channel %s: dropped %s message: %v", channel, fqn, err)
		return nil, false
	}
	js, err := protojson.Marshal(msg)
	if err != nil {
		g.typedEventSkip("decode", "ps.sub: channel %s: dropped %s message: %v", channel, fqn, err)
		return nil, false
	}
	raw, err := proto.Marshal(&psv1.Event{
		Channel:     channel,
		Payload:     string(js),
		PayloadType: fqn,
		Ts:          time.Now().Unix(),
	})
	if err != nil {
		return nil, false
	}
	ev := acquireDynamicMessage(outputDesc)
	defer releaseDynamicMessage(outputDesc, ev)
	if err := proto.Unmarshal(raw, ev); err != nil {
		return nil, false
	}
	return messageToMap(ev), true
}

func (g *Gateway) typedEventSkip(reason, format string, args ...any) {
	if pm := unwrapPrometheusMetrics(g.cfg.metrics); pm != nil {
		pm.recordPubSubTypedSkip(reason)
	}
	g.typedSkipLog.logf(g, format, args...)
}

// publishTyped is ps.pub on a named channel: payload is protojson of
// payloadType (required on a union channel, else the bound type),
// published as binary proto with the typed-event headers and a random
// message id.
func (g *Gateway) publishTyped(c *eventChannel, subject, payload, payloadType string) error {
	if payloadType == "" {
		if c.union() {
			return Reject(CodeInvalidArgument, fmt.Sprintf("ps.pub: channel %s carries several types; payloadType required (one of %s)", c.key, strings.Join(c.types, ", ")))
		}
		payloadType = c.types[0]
	}
	desc := c.descs[payloadType]
	if desc == nil {
		if containsString(c.types, payloadType) {
			return fmt.Errorf("ps.pub: channel %s: no descriptor for %s", c.key, payloadType)
		}
		return Reject(CodeInvalidArgument, fmt.Sprintf("ps.pub: channel %s does not carry %s (one of %s)", c.key, payloadType, strings.Join(c.types, ", ")))
	}
	msg := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(payload), msg); err != nil {
		return Reject(CodeInvalidArgument, fmt.Sprintf("ps.pub: payload for channel %s is not %s: %v", c.key, payloadType, err))
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ps.pub: marshal %s: %w", payloadType, err)
	}
	hb, ok := g.psBus.(gat.HeaderBus)
	if !ok {
		return fmt.Errorf("ps.pub: bus does not carry headers; cannot publish typed event on %s", c.key)
	}
	h := gat.Header{}
	h.Set(EventHeaderType, payloadType)
	h.Set(EventHeaderChannel, c.key)
	h.Set(EventHeaderMsgID, newEventMsgID())
	if err := hb.PublishMsg(subject, h, body); err != nil {
		return fmt.Errorf("ps.pub: publish: %w", err)
	}
	return nil
}

func newEventMsgID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// skipLogger writes at most one line per skipLogEvery; lines in
// between are counted and the count is reported with the next line.
type skipLogger struct {
	mu      sync.Mutex
	last    time.Time
	dropped int
}

const skipLogEvery = 10 * time.Second

func (l *skipLogger) logf(g *Gateway, format string, args ...any) {
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < skipLogEvery {
		l.dropped++
		l.mu.Unlock()
		return
	}
	n := l.dropped
	l.last, l.dropped = now, 0
	l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if n > 0 {
		msg += fmt.Sprintf(" (%d more since the last line)", n)
	}
	if g.cfg.cluster != nil && g.cfg.cluster.Server != nil {
		g.cfg.cluster.Server.Warnf("gateway: %s", msg)
		return
	}
	fmt.Println("gateway: " + msg)
}
