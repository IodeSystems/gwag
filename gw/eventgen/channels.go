package eventgen

import (
	"fmt"
	"regexp"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/events"
)

// ChannelKind is the shape of a channel's payload.
//
// Stability: experimental
type ChannelKind int

const (
	// KindPlain: one message.
	KindPlain ChannelKind = iota
	// KindOneof: one message whose fields are all in one oneof.
	// Handlers can take the whole message or one per case.
	KindOneof
	// KindUnion: several messages in one package with the same name and
	// pattern; Gwag-Type says which one a message is.
	KindUnion
)

// Channel is one named channel. Language emitters read it.
//
// Stability: experimental
type Channel struct {
	// Key is "<package>.<Name>".
	Key     string
	Package string
	Name    string
	// Pattern is the parsed pattern; Pattern.String() is as written.
	Pattern events.Pattern
	Kind    ChannelKind
	// Message is the payload message (KindPlain, KindOneof).
	Message protoreflect.MessageDescriptor
	// Oneof is the payload's oneof (KindOneof).
	Oneof protoreflect.OneofDescriptor
	// Members are the union's messages in declaration order (KindUnion).
	Members []protoreflect.MessageDescriptor
	// File declares the channel's first message; emitters put the
	// channel's code with this file.
	File protoreflect.FileDescriptor
	// Stream is events.StreamName(Key).
	Stream string
}

const (
	bindingExt   = "gwag.ps.v1.binding"
	bindingPat   = "pattern"
	bindingName  = "name"
	channelNameP = `^[A-Za-z][A-Za-z0-9_]*$`
)

var channelNameRE = regexp.MustCompile(channelNameP)

// binding reads (gwag.ps.v1.binding) off md. Works for dynamicpb
// option values (protocompile) and the generated type alike.
func binding(md protoreflect.MessageDescriptor) (name, pattern string, ok bool) {
	opts := md.Options()
	if opts == nil {
		return "", "", false
	}
	opts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !fd.IsExtension() || string(fd.FullName()) != bindingExt {
			return true
		}
		m := v.Message()
		if f := m.Descriptor().Fields().ByName(bindingPat); f != nil {
			pattern = m.Get(f).String()
		}
		if f := m.Descriptor().Fields().ByName(bindingName); f != nil {
			name = m.Get(f).String()
		}
		ok = true
		return false
	})
	return name, pattern, ok
}

// Binding is a channel binding given outside the proto, for a message
// whose file has no (gwag.ps.v1.binding): a schema imported from a
// registry.
//
// Stability: experimental
type Binding struct {
	Name    string
	Pattern string
}

// Channels returns the named channels declared in files, in
// declaration order. Messages without a binding name are not
// channels. Errors: a bad name or pattern, or one name with two
// patterns in a package.
//
// Stability: experimental
func Channels(files []protoreflect.FileDescriptor) ([]*Channel, error) {
	return ChannelsWith(files, nil)
}

// ChannelsWith is Channels where extra, keyed by message full name,
// binds messages the proto does not. extra wins over the proto.
//
// Stability: experimental
func ChannelsWith(files []protoreflect.FileDescriptor, extra map[string]Binding) ([]*Channel, error) {
	var out []*Channel
	byKey := map[string]*Channel{}
	add := func(md protoreflect.MessageDescriptor) error {
		name, pattern, ok := binding(md)
		if b, has := extra[string(md.FullName())]; has {
			name, pattern, ok = b.Name, b.Pattern, true
		}
		if !ok || name == "" {
			return nil
		}
		if !channelNameRE.MatchString(name) {
			return fmt.Errorf("%s: channel name %q: want %s", md.FullName(), name, channelNameP)
		}
		pkg := string(md.ParentFile().Package())
		key := name
		if pkg != "" {
			key = pkg + "." + name
		}
		if c := byKey[key]; c != nil {
			if c.Pattern.String() != pattern {
				first := c.Message
				if c.Kind == KindUnion {
					first = c.Members[0]
				}
				return fmt.Errorf("channel %s: %s has pattern %q but %s has %q; messages sharing a channel name must share the pattern",
					key, first.FullName(), c.Pattern.String(), md.FullName(), pattern)
			}
			if c.Kind != KindUnion {
				c.Members = []protoreflect.MessageDescriptor{c.Message}
				c.Kind, c.Message, c.Oneof = KindUnion, nil, nil
			}
			c.Members = append(c.Members, md)
			return nil
		}
		p, err := events.ParsePattern(pattern)
		if err != nil {
			return fmt.Errorf("%s: %w", md.FullName(), err)
		}
		c := &Channel{Key: key, Package: pkg, Name: name, Pattern: p, Kind: KindPlain, Message: md, File: md.ParentFile(), Stream: events.StreamName(key)}
		if od := soleOneof(md); od != nil {
			c.Kind, c.Oneof = KindOneof, od
		}
		byKey[key] = c
		out = append(out, c)
		return nil
	}
	var walk func(protoreflect.MessageDescriptors) error
	walk = func(ms protoreflect.MessageDescriptors) error {
		for i := 0; i < ms.Len(); i++ {
			if err := add(ms.Get(i)); err != nil {
				return err
			}
			if err := walk(ms.Get(i).Messages()); err != nil {
				return err
			}
		}
		return nil
	}
	for _, f := range files {
		if err := walk(f.Messages()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// soleOneof returns md's oneof when md has fields and every one of them
// is in that one (non-synthetic) oneof.
func soleOneof(md protoreflect.MessageDescriptor) protoreflect.OneofDescriptor {
	fields := md.Fields()
	if fields.Len() == 0 {
		return nil
	}
	var od protoreflect.OneofDescriptor
	for i := 0; i < fields.Len(); i++ {
		o := fields.Get(i).ContainingOneof()
		if o == nil || o.IsSynthetic() {
			return nil
		}
		if od == nil {
			od = o
		} else if od != o {
			return nil
		}
	}
	return od
}
