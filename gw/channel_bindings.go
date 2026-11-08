package gateway

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// channelBindingEntry is one aggregated binding with its owning slot
// coordinate. Used by the gateway-wide index for publish-time
// payload_type stamping and admin enumeration.
type channelBindingEntry struct {
	pattern     string // normalized: named segments read as `*`
	declared    string // as declared, named segments kept
	messageFQN  string
	name        string // "" for an unnamed binding
	channel     string // channel key; "" for an unnamed binding
	namespace   string
	version     string
	messageDesc protoreflect.MessageDescriptor
}

// eventChannel is one named channel: every binding of one slot that
// shares a channel key. More than one type = a union channel; the
// Gwag-Type header says which type a message carries.
type eventChannel struct {
	key       string // "<proto package>.<name>"
	pattern   string // normalized
	declared  string
	namespace string
	version   string
	types     []string // message FQNs, declaration order
	descs     map[string]protoreflect.MessageDescriptor
}

func (c *eventChannel) union() bool { return len(c.types) > 1 }

// streamName is the JetStream stream that stores the channel.
func (c *eventChannel) streamName() string { return eventStreamName(c.key) }

// eventStreamName is "EV_" + the channel key with "." replaced by "_"
// (orders.v1.OrderEvents → EV_orders_v1_OrderEvents).
func eventStreamName(key string) string {
	return "EV_" + strings.ReplaceAll(key, ".", "_")
}

// channelBindingIndex is the gateway-wide aggregation of all slot
// channel bindings. Rebuilt on every assembleLocked and atomic-
// swapped so psPub can look up payload_type without holding g.mu.
type channelBindingIndex struct {
	entries  []channelBindingEntry
	channels []*eventChannel // sorted by key
	byKey    map[string]*eventChannel
	// msgDescs holds every message of every registered proto slot, so
	// a Gwag-Type header can name any of them.
	msgDescs map[string]protoreflect.MessageDescriptor
}

// lookupPayloadType returns the MessageFQN for the first binding
// pattern that matches the given channel subject, or "" if no match.
// Uses NATS-style pattern matching (same grammar as WithChannelAuth).
func (idx *channelBindingIndex) lookupPayloadType(channel string) string {
	if idx == nil {
		return ""
	}
	for _, e := range idx.entries {
		if subjectMatchesPattern(e.pattern, channel) {
			return e.messageFQN
		}
	}
	return ""
}

// lookupChannel returns the named channel whose pattern matches the
// subject, or nil (no binding matches, or the first match is
// unnamed).
func (idx *channelBindingIndex) lookupChannel(subject string) *eventChannel {
	e := idx.lookupBinding(subject)
	if e == nil || e.channel == "" {
		return nil
	}
	return idx.byKey[e.channel]
}

// messageDesc returns the descriptor of a registered message, or nil.
func (idx *channelBindingIndex) messageDesc(fqn string) protoreflect.MessageDescriptor {
	if idx == nil {
		return nil
	}
	return idx.msgDescs[fqn]
}

// lookupBinding returns the full binding entry for the first pattern
// that matches the channel, or nil. Used by the enforcement path to
// access the message descriptor for payload validation.
func (idx *channelBindingIndex) lookupBinding(channel string) *channelBindingEntry {
	if idx == nil {
		return nil
	}
	for i := range idx.entries {
		if subjectMatchesPattern(idx.entries[i].pattern, channel) {
			return &idx.entries[i]
		}
	}
	return nil
}

// validatePayload attempts to parse the payload JSON as the bound
// proto message. Returns nil if the payload is valid, or a Reject
// error with CodeInvalidArgument if parsing fails.
func (idx *channelBindingIndex) validatePayload(channel, payload string) error {
	binding := idx.lookupBinding(channel)
	if binding == nil || binding.messageDesc == nil {
		return nil
	}
	msg := dynamicpb.NewMessage(binding.messageDesc)
	if err := protojson.Unmarshal([]byte(payload), msg); err != nil {
		return Reject(CodeInvalidArgument, fmt.Sprintf("ps.pub: payload for channel %q does not match bound type %s: %v", channel, binding.messageFQN, err))
	}
	return nil
}

// rebuildChannelBindingIndexLocked aggregates channelBindings from
// every slot into a fresh index and atomic-swaps it. Also resolves
// message descriptors from registered proto slots for payload
// validation. Caller holds g.mu.
func (g *Gateway) rebuildChannelBindingIndexLocked() {
	// Build a message FQN → descriptor lookup from all proto slots.
	msgDescs := g.resolveMessageDescriptorsLocked()
	idx := &channelBindingIndex{byKey: map[string]*eventChannel{}, msgDescs: msgDescs}
	keys := make([]poolKey, 0, len(g.slots))
	for k := range g.slots {
		keys = append(keys, k)
	}
	// Stable order so first-match lookups do not depend on map order.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		return keys[i].version < keys[j].version
	})
	for _, k := range keys {
		s := g.slots[k]
		for _, b := range s.channelBindings {
			entry := channelBindingEntry{
				pattern:    normalizeSubject(b.Pattern),
				declared:   b.Pattern,
				messageFQN: b.MessageFQN,
				name:       b.Name,
				channel:    b.Channel,
				namespace:  s.key.namespace,
				version:    s.key.version,
			}
			if desc, ok := msgDescs[b.MessageFQN]; ok {
				entry.messageDesc = desc
			}
			idx.entries = append(idx.entries, entry)
			if b.Channel == "" {
				continue
			}
			c := idx.byKey[b.Channel]
			if c == nil {
				c = &eventChannel{
					key:       b.Channel,
					pattern:   entry.pattern,
					declared:  b.Pattern,
					namespace: s.key.namespace,
					version:   s.key.version,
					descs:     map[string]protoreflect.MessageDescriptor{},
				}
				idx.byKey[b.Channel] = c
				idx.channels = append(idx.channels, c)
			}
			c.types = append(c.types, b.MessageFQN)
			if entry.messageDesc != nil {
				c.descs[b.MessageFQN] = entry.messageDesc
			}
		}
	}
	sort.Slice(idx.channels, func(i, j int) bool { return idx.channels[i].key < idx.channels[j].key })
	g.channelBindingIndex.Store(idx)
	g.ensureEventStreamsAsync(idx.channels)
}

// resolveMessageDescriptorsLocked walks all proto and internal-proto
// slots and returns a map from message FQN to MessageDescriptor.
// Used by rebuildChannelBindingIndexLocked to resolve bound types
// for payload validation. Caller holds g.mu.
func (g *Gateway) resolveMessageDescriptorsLocked() map[string]protoreflect.MessageDescriptor {
	out := make(map[string]protoreflect.MessageDescriptor)
	for _, s := range g.slots {
		var fd protoreflect.FileDescriptor
		switch {
		case s.proto != nil:
			fd = s.proto.file
		case s.internalProto != nil:
			fd = s.internalProto.file
		default:
			continue
		}
		walkMessagesForDescriptors(fd, out)
	}
	return out
}

func walkMessagesForDescriptors(fd protoreflect.FileDescriptor, out map[string]protoreflect.MessageDescriptor) {
	var walk func(protoreflect.MessageDescriptors)
	walk = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			md := ms.Get(i)
			out[string(md.FullName())] = md
			walk(md.Messages())
		}
	}
	walk(fd.Messages())
}

// channelBindingIndexSnapshot returns a copy of the current binding
// index entries for admin enumeration. Does NOT hold g.mu — the
// index is atomic-loaded.
func (g *Gateway) channelBindingIndexSnapshot() []channelBindingEntry {
	idx := g.channelBindingIndex.Load()
	if idx == nil {
		return nil
	}
	out := make([]channelBindingEntry, len(idx.entries))
	copy(out, idx.entries)
	return out
}

// channelBindingExtensionFullName is the proto full name of the
// `(gwag.ps.binding)` extension defined in
// `gw/proto/ps/v1/options.proto`. We compare by name rather than by
// the generated `psv1.E_Binding` extension type because protocompile
// resolves options against its own descriptor pool — the option's
// value comes back as `*dynamicpb.Message`, not the concrete
// `*psv1.ChannelBinding`, so `proto.GetExtension` would panic on the
// type assertion. The protoreflect Range over MessageOptions sees the
// same field regardless of which pool the descriptor came from.
const channelBindingExtensionFullName = "gwag.ps.v1.binding"

// channelBindingPatternFieldName / channelBindingNameFieldName are
// the fields on `ChannelBinding` we extract.
const (
	channelBindingPatternFieldName = "pattern"
	channelBindingNameFieldName    = "name"
)

// extractChannelBindings walks every message reachable from `fd`
// (top-level + nested, but not imported files — bindings travel with
// their declaring file's slot) and returns the `(gwag.ps.binding)`
// extensions stamped on them. Pattern is the literal NATS-style
// pattern the option declared; MessageFQN is the message's proto
// full name; Name / Channel are set for a named binding.
//
// Messages without the option, and proto files that don't import
// `gw/proto/ps/v1/options.proto`, yield no entries. Idempotent across
// rebakes — the IR slice is rebuilt from scratch each call.
//
// Errors (validateChannelBindings): a malformed pattern, a name
// without a pattern, one name with two patterns, or one pattern with
// two names.
func extractChannelBindings(fd protoreflect.FileDescriptor) ([]ir.ChannelBinding, error) {
	if fd == nil {
		return nil, nil
	}
	var out []ir.ChannelBinding
	if err := walkMessagesForBindings(fd.Messages(), string(fd.Package()), &out); err != nil {
		return nil, err
	}
	if err := validateChannelBindings(out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkMessagesForBindings(ms protoreflect.MessageDescriptors, pkg string, out *[]ir.ChannelBinding) error {
	for i := 0; i < ms.Len(); i++ {
		md := ms.Get(i)
		pattern, name := bindingOnMessage(md)
		switch {
		case pattern != "":
			b := ir.ChannelBinding{
				Pattern:    pattern,
				MessageFQN: string(md.FullName()),
				Name:       name,
			}
			if name != "" {
				b.Channel = channelKey(pkg, name)
			}
			*out = append(*out, b)
		case name != "":
			return fmt.Errorf("channel binding on %s: name %q needs a pattern", md.FullName(), name)
		}
		if err := walkMessagesForBindings(md.Messages(), pkg, out); err != nil {
			return err
		}
	}
	return nil
}

// channelKey is "<proto package>.<name>", or name when the file has
// no package.
func channelKey(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// validateChannelBindings checks the bindings of one slot: every
// pattern parses (normalizeChannelPattern), a channel name is an
// identifier, one channel has one pattern, and one pattern has one
// channel. Several messages with the same name and pattern are a
// union channel. Unnamed bindings that share a pattern are allowed
// (as before named channels existed); the first one wins at lookup.
func validateChannelBindings(bs []ir.ChannelBinding) error {
	patternOf := map[string]string{} // channel → declared pattern
	channelOf := map[string]string{} // normalized pattern → channel
	for _, b := range bs {
		norm, _, err := normalizeChannelPattern(b.Pattern)
		if err != nil {
			return fmt.Errorf("channel binding on %s: %w", b.MessageFQN, err)
		}
		if b.Name != "" && !isIdent(b.Name) {
			return fmt.Errorf("channel binding on %s: name %q is not an identifier", b.MessageFQN, b.Name)
		}
		if b.Channel != "" {
			if p, ok := patternOf[b.Channel]; ok && p != b.Pattern {
				return fmt.Errorf("channel %s: %s declares pattern %q, another message declares %q (one channel, one pattern)", b.Channel, b.MessageFQN, b.Pattern, p)
			}
			patternOf[b.Channel] = b.Pattern
		}
		if c, ok := channelOf[norm]; ok && c != b.Channel {
			return fmt.Errorf("channel binding pattern %q: %s names channel %q, another message names %q (one pattern, one channel name)", b.Pattern, b.MessageFQN, b.Channel, c)
		}
		channelOf[norm] = b.Channel
	}
	return nil
}

// normalizeChannelPattern validates a binding pattern and returns it
// with each named segment (`{orderId}`) replaced by `*`, plus the
// segment names in order. A segment containing a brace must be
// exactly `{ident}`; names are unique within a pattern. `*` and `>`
// keep their NATS meaning; `>` only as the last segment.
func normalizeChannelPattern(p string) (string, []string, error) {
	if p == "" {
		return "", nil, fmt.Errorf("empty pattern")
	}
	segs := strings.Split(p, ".")
	var names []string
	seen := map[string]bool{}
	for i, seg := range segs {
		switch {
		case seg == "":
			return "", nil, fmt.Errorf("pattern %q: empty segment", p)
		case seg == ">":
			if i != len(segs)-1 {
				return "", nil, fmt.Errorf("pattern %q: `>` must be the last segment", p)
			}
		case strings.ContainsAny(seg, "{}"):
			if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' || !isIdent(seg[1:len(seg)-1]) {
				return "", nil, fmt.Errorf("pattern %q: malformed named segment %q (want {name})", p, seg)
			}
			n := seg[1 : len(seg)-1]
			if seen[n] {
				return "", nil, fmt.Errorf("pattern %q: segment name %q used twice", p, n)
			}
			seen[n] = true
			names = append(names, n)
			segs[i] = "*"
		case strings.ContainsAny(seg, "*>") && len(seg) > 1:
			return "", nil, fmt.Errorf("pattern %q: wildcard inside segment %q", p, seg)
		}
	}
	return strings.Join(segs, "."), names, nil
}

// normalizeSubject maps named segments (`{x}`) to `*` without
// validating; a subject without braces is returned unchanged. Used
// wherever a pattern may carry the binding grammar: matching, auth
// rules, ps.sub patterns, stream subjects.
func normalizeSubject(p string) string {
	if !strings.ContainsRune(p, '{') {
		return p
	}
	segs := strings.Split(p, ".")
	for i, seg := range segs {
		if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			segs[i] = "*"
		}
	}
	return strings.Join(segs, ".")
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// bindingOnMessage returns the pattern and name on the
// `(gwag.ps.binding)` extension if it's set on md's MessageOptions,
// or "", "" if no binding is declared. Works against both dynamicpb
// option values (protocompile) and the generated concrete type
// (boot-time Go-registered extension).
func bindingOnMessage(md protoreflect.MessageDescriptor) (pattern, name string) {
	opts := md.Options()
	if opts == nil {
		return "", ""
	}
	opts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !fd.IsExtension() || string(fd.FullName()) != channelBindingExtensionFullName {
			return true
		}
		bindingMsg := v.Message()
		fields := bindingMsg.Descriptor().Fields()
		if f := fields.ByName(channelBindingPatternFieldName); f != nil {
			pattern = bindingMsg.Get(f).String()
		}
		if f := fields.ByName(channelBindingNameFieldName); f != nil {
			name = bindingMsg.Get(f).String()
		}
		return false
	})
	return pattern, name
}

// WithChannelBinding registers a channel→payload-type pairing at the
// gateway level, for non-proto adopters and gateway-shipped defaults.
// Pattern uses NATS-style wildcards (same grammar as WithChannelAuth).
// MessageFQN is the proto fully-qualified message name
// ("<package>.<Name>").
//
// Multiple calls compose; declaration order is preserved. Bindings are
// applied to the gateway-internal ps slot during New() and route
// through the same tier/uniqueness policy as proto-declarative
// bindings — cross-slot pattern conflicts are rejected, and vN slots
// lock by (pattern, payload_fqn).
//
// Unlike the proto-declarative path (which extracts bindings from the
// `(gwag.ps.binding)` message option at slot bake time), this API
// accepts the FQN string directly, avoiding the need for the caller
// to have the proto message type available at gateway construction.
//
// Stability: stable
func WithChannelBinding(pattern, messageFQN string) Option {
	return func(cfg *config) {
		cfg.channelBindings = append(cfg.channelBindings, ir.ChannelBinding{
			Pattern:    pattern,
			MessageFQN: messageFQN,
		})
	}
}

// applyRuntimeBindingsLocked merges runtime-declared bindings
// (WithChannelBinding) into the gateway-internal ps slot. Runs the
// cross-slot uniqueness check before mutating. Caller does NOT hold
// g.mu — acquires it internally.
func (g *Gateway) applyRuntimeBindingsLocked(bindings []ir.ChannelBinding) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	psKey := poolKey{namespace: psNamespace, version: psVersion}
	psSlot, ok := g.slots[psKey]
	if !ok {
		return fmt.Errorf("channel binding: ps slot %s/%s not installed", psNamespace, psVersion)
	}

	for _, b := range bindings {
		if _, _, err := normalizeChannelPattern(b.Pattern); err != nil {
			return fmt.Errorf("channel binding %s: %w", b.MessageFQN, err)
		}
	}
	// Check cross-slot uniqueness before mutating.
	if err := g.checkCrossSlotBindingsLocked(psKey, bindings); err != nil {
		return err
	}

	psSlot.channelBindings = append(psSlot.channelBindings, bindings...)
	g.bakeSlotIRLocked(psSlot)
	return g.assembleLocked()
}

// WithChannelBindingEnforce enables shape strictness for ps.pub: when
// a channel has a binding, the payload is parsed as the bound proto
// message and rejected with InvalidArgument if it doesn't match.
// Default false (permissive). See WithStrictPayloadTypes for coverage
// strictness. Both can be enabled together.
//
// Stability: stable
func WithChannelBindingEnforce() Option {
	return func(cfg *config) { cfg.psBindingEnforce = true }
}

// WithStrictPayloadTypes enables coverage strictness for ps.pub:
// rejects publishes to channels that match no WithChannelBinding
// pattern (where payload_type would otherwise be blank). Default
// false (permissive) so the open tier stays usable for ad-hoc / dev
// channels. See WithChannelBindingEnforce for shape strictness. Both
// can be enabled together.
//
// Stability: stable
func WithStrictPayloadTypes() Option {
	return func(cfg *config) { cfg.psStrictPayloadTypes = true }
}
