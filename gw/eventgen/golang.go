package eventgen

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func init() {
	Register(&Language{Name: "go", Plugins: goPlugins, Emitter: EmitterFunc(emitGo)})
}

// goPlugins resolves protoc-gen-go (and protoc-gen-go-grpc with
// --grpc) as `go tool` entries of the consumer's go.mod.
func goPlugins(ctx context.Context, env Env) ([]Plugin, error) {
	pg, err := ResolvePlugin(ctx, "protoc-gen-go", []string{"go", "tool", "protoc-gen-go"}, env.ModuleDir,
		"go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@"+depVersion("google.golang.org/protobuf"))
	if err != nil {
		return nil, err
	}
	out := []Plugin{pg}
	if env.Grpc {
		pgg, err := ResolvePlugin(ctx, "protoc-gen-go-grpc", []string{"go", "tool", "protoc-gen-go-grpc"}, env.ModuleDir,
			"go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest")
		if err != nil {
			return nil, err
		}
		out = append(out, pgg)
	}
	return out, nil
}

var (
	eventsPkg  = protogen.GoImportPath("github.com/iodesystems/gwag/v2/events")
	contextPkg = protogen.GoImportPath("context")
	protoPkg   = protogen.GoImportPath("google.golang.org/protobuf/proto")
)

// emitGo writes <prefix>.events.go next to each .pb.go whose file
// declares a channel. protogen reads the same parameter protoc-gen-go
// got, so paths and package names match.
func emitGo(in *Input) ([]*pluginpb.CodeGeneratorResponse_File, error) {
	if len(in.Channels) == 0 {
		return nil, nil
	}
	plugin, err := protogen.Options{ParamFunc: func(string, string) error { return nil }}.New(in.Request)
	if err != nil {
		return nil, err
	}
	msgs := map[protoreflect.FullName]*protogen.Message{}
	var index func([]*protogen.Message)
	index = func(ms []*protogen.Message) {
		for _, m := range ms {
			msgs[m.Desc.FullName()] = m
			index(m.Messages)
		}
	}
	for _, f := range plugin.Files {
		index(f.Messages)
	}

	byFile := map[string][]*Channel{}
	for _, c := range in.Channels {
		byFile[c.File.Path()] = append(byFile[c.File.Path()], c)
	}
	pkgFile := map[protogen.GoImportPath]string{}
	for _, f := range plugin.Files {
		chans := byFile[f.Desc.Path()]
		if !f.Generate || len(chans) == 0 {
			continue
		}
		if prev, dup := pkgFile[f.GoImportPath]; dup {
			return nil, fmt.Errorf("%s and %s both declare channels in Go package %s; each generates a Bus type, so declare a package's channels in one .proto file",
				prev, f.Desc.Path(), f.GoImportPath)
		}
		pkgFile[f.GoImportPath] = f.Desc.Path()
		if err := goFile(plugin, f, chans, msgs, in.Header); err != nil {
			return nil, err
		}
	}
	resp := plugin.Response()
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.GetError())
	}
	return resp.File, nil
}

// goChannel is a channel with its Go names worked out.
type goChannel struct {
	*Channel
	goName   string // OrderEvents
	varName  string // orderEventsChannel
	subject  string // OrderEventsSubject
	segs     []string
	msg      *protogen.Message   // plain, oneof
	oneof    *protogen.Oneof     // oneof
	members  []*protogen.Message // union
	handlers string              // OrderEventsHandlers
	event    string              // OrderEventsEvent
	marker   string              // isOrderEventsEvent
}

func goFile(plugin *protogen.Plugin, f *protogen.File, chans []*Channel, msgs map[protoreflect.FullName]*protogen.Message, h Header) error {
	taken := goPackageIdents(plugin, f.GoImportPath)
	claim := func(c *Channel, name string) error {
		if prev, ok := taken[name]; ok {
			return fmt.Errorf("channel %s: generated name %s collides with %s; rename the channel", c.Key, name, prev)
		}
		taken[name] = "channel " + c.Key
		return nil
	}
	for _, n := range []string{"Bus", "NewBus"} {
		if prev, ok := taken[n]; ok {
			return fmt.Errorf("%s: generated name %s collides with %s", f.Desc.Path(), n, prev)
		}
	}

	var gcs []*goChannel
	for _, c := range chans {
		gc := &goChannel{Channel: c, goName: upperFirst(c.Name)}
		gc.varName = lowerFirst(gc.goName) + "Channel"
		gc.subject = gc.goName + "Subject"
		gc.handlers = gc.goName + "Handlers"
		gc.event = gc.goName + "Event"
		gc.marker = "is" + gc.goName + "Event"
		names := []string{gc.subject, "Publish" + gc.goName, "On" + gc.goName}
		seen := map[string]bool{}
		for _, s := range c.Pattern.Segments() {
			n := goCamel(s.Name)
			if seen[n] {
				return fmt.Errorf("channel %s: segments name the same Go field %s", c.Key, n)
			}
			seen[n] = true
			gc.segs = append(gc.segs, n)
		}
		switch c.Kind {
		case KindPlain:
			gc.msg = msgs[c.Message.FullName()]
		case KindOneof:
			gc.msg = msgs[c.Message.FullName()]
			for _, o := range gc.msg.Oneofs {
				if o.Desc.FullName() == c.Oneof.FullName() {
					gc.oneof = o
				}
			}
			for _, fl := range gc.oneof.Fields {
				if fl.GoName == "Unknown" {
					return fmt.Errorf("channel %s: oneof case %s maps to Go name Unknown, which the handler struct uses", c.Key, fl.Desc.Name())
				}
			}
			names = append(names, gc.handlers, "On"+gc.goName+"Message")
		case KindUnion:
			for _, md := range c.Members {
				m := msgs[md.FullName()]
				if m.GoIdent.GoImportPath != f.GoImportPath {
					return fmt.Errorf("channel %s: member %s is in Go package %s, not %s; union members must share a Go package",
						c.Key, md.FullName(), m.GoIdent.GoImportPath, f.GoImportPath)
				}
				if m.GoIdent.GoName == "Unknown" {
					return fmt.Errorf("channel %s: member %s has Go name Unknown, which the handler struct uses", c.Key, md.FullName())
				}
				gc.members = append(gc.members, m)
			}
			names = append(names, gc.handlers, gc.event)
		}
		for _, n := range names {
			if err := claim(c, n); err != nil {
				return err
			}
		}
		gcs = append(gcs, gc)
	}

	g := plugin.NewGeneratedFile(f.GeneratedFilenamePrefix+".events.go", f.GoImportPath)
	g.P("// Code generated by gwag gen. DO NOT EDIT.")
	g.P("// versions:")
	for _, l := range h.Lines() {
		g.P("// \t", l)
	}
	g.P("// source: ", f.Desc.Path())
	g.P()
	g.P("package ", f.GoPackageName)
	g.P()

	core := g.QualifiedGoIdent(eventsPkg.Ident("Core"))
	backend := g.QualifiedGoIdent(eventsPkg.Ident("Backend"))
	option := g.QualifiedGoIdent(eventsPkg.Ident("Option"))
	subscription := g.QualifiedGoIdent(eventsPkg.Ident("Subscription"))
	g.P("// Bus publishes and listens on the event channels declared in")
	g.P("// ", f.Desc.Path(), ".")
	g.P("type Bus struct {")
	g.P("core *", core)
	g.P("}")
	g.P()
	g.P("// NewBus returns a Bus over backend b.")
	g.P("func NewBus(b ", backend, ", opts ...", option, ") *Bus {")
	g.P("return &Bus{core: ", eventsPkg.Ident("NewCore"), "(b, opts...)}")
	g.P("}")
	g.P()
	g.P("// Subscriptions returns the (channel, group) pairs this bus listens on.")
	g.P("// Pass them to controlclient's Service.Subscribes.")
	g.P("func (b *Bus) Subscriptions() []", subscription, " { return b.core.Subscriptions() }")
	g.P()
	g.P("// Core returns the underlying events.Core.")
	g.P("func (b *Bus) Core() *", core, " { return b.core }")
	g.P()
	for _, gc := range gcs {
		goChannelCode(g, gc)
	}
	return nil
}

// goPackageIdents returns the exported top-level names protoc-gen-go
// declares in the Go package, for collision checks.
func goPackageIdents(plugin *protogen.Plugin, pkg protogen.GoImportPath) map[string]string {
	out := map[string]string{}
	var msgs func([]*protogen.Message)
	enums := func(es []*protogen.Enum) {
		for _, e := range es {
			out[e.GoIdent.GoName] = "enum " + string(e.Desc.FullName())
			for _, v := range e.Values {
				out[v.GoIdent.GoName] = "enum value " + string(v.Desc.FullName())
			}
		}
	}
	msgs = func(ms []*protogen.Message) {
		for _, m := range ms {
			out[m.GoIdent.GoName] = "message " + string(m.Desc.FullName())
			for _, o := range m.Oneofs {
				for _, fl := range o.Fields {
					out[fl.GoIdent.GoName] = "oneof case " + string(fl.Desc.FullName())
				}
			}
			enums(m.Enums)
			msgs(m.Messages)
		}
	}
	for _, f := range plugin.Files {
		if f.GoImportPath != pkg {
			continue
		}
		enums(f.Enums)
		msgs(f.Messages)
		for _, s := range f.Services {
			for _, suffix := range []string{"Client", "Server"} {
				out[s.GoName+suffix] = "service " + string(s.Desc.FullName())
			}
		}
	}
	return out
}

func goChannelCode(g *protogen.GeneratedFile, gc *goChannel) {
	ctxT := g.QualifiedGoIdent(contextPkg.Ident("Context"))
	msgT := g.QualifiedGoIdent(eventsPkg.Ident("Message"))
	pubOpt := g.QualifiedGoIdent(eventsPkg.Ident("PublishOption"))
	subOpt := g.QualifiedGoIdent(eventsPkg.Ident("SubscribeOption"))
	unmarshal := g.QualifiedGoIdent(protoPkg.Ident("Unmarshal"))
	pat := gc.Pattern.String()

	g.P("// ", gc.varName, " is channel ", gc.Key, ", pattern ", fmt.Sprintf("%q", pat), ".")
	g.P("var ", gc.varName, " = ", eventsPkg.Ident("MustChannel"), "(", fmt.Sprintf("%q, %q", gc.Key, pat), ")")
	g.P()

	g.P("// ", gc.subject, " holds the wildcard segments of a subject on channel ", gc.Key)
	g.P("// (pattern ", fmt.Sprintf("%q", pat), ").")
	if len(gc.segs) == 0 {
		g.P("type ", gc.subject, " struct{}")
		g.P()
		g.P("func (", gc.subject, ") segments() []string { return nil }")
		g.P()
		g.P("func new", gc.subject, "([]string) ", gc.subject, " { return ", gc.subject, "{} }")
	} else {
		g.P("type ", gc.subject, " struct {")
		for _, s := range gc.segs {
			g.P(s, " string")
		}
		g.P("}")
		g.P()
		g.P("func (s ", gc.subject, ") segments() []string {")
		g.P("return []string{", "s."+strings.Join(gc.segs, ", s."), "}")
		g.P("}")
		g.P()
		g.P("func new", gc.subject, "(v []string) ", gc.subject, " {")
		g.P("return ", gc.subject, "{")
		for i, s := range gc.segs {
			g.P(s, ": v[", i, "],")
		}
		g.P("}")
		g.P("}")
	}
	g.P()

	switch gc.Kind {
	case KindPlain, KindOneof:
		mt := g.QualifiedGoIdent(gc.msg.GoIdent)
		fqn := string(gc.msg.Desc.FullName())
		onName := "On" + gc.goName
		if gc.Kind == KindOneof {
			onName += "Message"
		}
		g.P("// Publish", gc.goName, " publishes m on channel ", gc.Key, ".")
		g.P("func (b *Bus) Publish", gc.goName, "(ctx ", ctxT, ", s ", gc.subject, ", m *", mt, ", opts ...", pubOpt, ") error {")
		g.P("return b.core.Publish(ctx, ", gc.varName, ", s.segments(), m, opts...)")
		g.P("}")
		g.P()
		g.P("// ", onName, " listens on channel ", gc.Key, " as group. Subscribers")
		g.P("// sharing a group share its messages. A nil error from h acks the")
		g.P("// message; an error redelivers it.")
		g.P("func (b *Bus) ", onName, "(ctx ", ctxT, ", group string, h func(", ctxT, ", ", gc.subject, ", *", mt, ") error, opts ...", subOpt, ") (stop func(), err error) {")
		g.P("return b.core.Subscribe(ctx, ", gc.varName, ", group, func(ctx ", ctxT, ", seg []string, msg *", msgT, ") error {")
		g.P("if t := msg.Type(); t != \"\" && t != ", fmt.Sprintf("%q", fqn), " {")
		g.P("b.core.Skip(", gc.varName, ", msg, ", fmt.Sprintf("%q", "not "+fqn), ")")
		g.P("return nil")
		g.P("}")
		g.P("m := new(", mt, ")")
		g.P("if err := ", unmarshal, "(msg.Data, m); err != nil {")
		g.P("return err")
		g.P("}")
		g.P("return h(ctx, new", gc.subject, "(seg), m)")
		g.P("}, opts...)")
		g.P("}")
		g.P()
		if gc.Kind == KindOneof {
			goOneofCode(g, gc, mt)
		}
	case KindUnion:
		goUnionCode(g, gc)
	}
}

func goOneofCode(g *protogen.GeneratedFile, gc *goChannel, mt string) {
	ctxT := g.QualifiedGoIdent(contextPkg.Ident("Context"))
	subOpt := g.QualifiedGoIdent(eventsPkg.Ident("SubscribeOption"))
	g.P("// ", gc.handlers, " handles each case of ", gc.msg.GoIdent.GoName, ".", gc.oneof.GoName, ". A nil")
	g.P("// field acks that case unhandled. Unknown gets a message with no case")
	g.P("// set, or one this code does not know; nil acks it unhandled.")
	g.P("type ", gc.handlers, " struct {")
	for _, fl := range gc.oneof.Fields {
		g.P(fl.GoName, " func(", ctxT, ", ", gc.subject, ", ", goFieldType(g, fl), ") error")
	}
	g.P("Unknown func(", ctxT, ", ", gc.subject, ", *", mt, ") error")
	g.P("}")
	g.P()
	g.P("// On", gc.goName, " listens on channel ", gc.Key, " as group and calls the")
	g.P("// handler for the message's ", gc.oneof.Desc.Name(), " case.")
	g.P("func (b *Bus) On", gc.goName, "(ctx ", ctxT, ", group string, h ", gc.handlers, ", opts ...", subOpt, ") (stop func(), err error) {")
	g.P("return b.On", gc.goName, "Message(ctx, group, func(ctx ", ctxT, ", s ", gc.subject, ", m *", mt, ") error {")
	g.P("r := m.ProtoReflect()")
	g.P("if fd := r.WhichOneof(r.Descriptor().Oneofs().ByName(", fmt.Sprintf("%q", gc.oneof.Desc.Name()), ")); fd != nil {")
	g.P("switch fd.Number() {")
	for _, fl := range gc.oneof.Fields {
		g.P("case ", fl.Desc.Number(), ":")
		g.P("if h.", fl.GoName, " == nil {")
		g.P("return nil")
		g.P("}")
		g.P("return h.", fl.GoName, "(ctx, s, m.Get", fl.GoName, "())")
	}
	g.P("}")
	g.P("}")
	g.P("if h.Unknown != nil {")
	g.P("return h.Unknown(ctx, s, m)")
	g.P("}")
	g.P("b.core.Skip(", gc.varName, ", ", eventsPkg.Ident("MessageFrom"), "(ctx), \"no case set, or no handler for it\")")
	g.P("return nil")
	g.P("}, opts...)")
	g.P("}")
	g.P()
}

func goUnionCode(g *protogen.GeneratedFile, gc *goChannel) {
	ctxT := g.QualifiedGoIdent(contextPkg.Ident("Context"))
	msgT := g.QualifiedGoIdent(eventsPkg.Ident("Message"))
	pubOpt := g.QualifiedGoIdent(eventsPkg.Ident("PublishOption"))
	subOpt := g.QualifiedGoIdent(eventsPkg.Ident("SubscribeOption"))
	unmarshal := g.QualifiedGoIdent(protoPkg.Ident("Unmarshal"))
	names := make([]string, len(gc.members))
	for i, m := range gc.members {
		names[i] = m.GoIdent.GoName
	}
	g.P("// ", gc.event, " is a message channel ", gc.Key, " carries:")
	g.P("// ", strings.Join(names, ", "), ".")
	g.P("type ", gc.event, " interface {")
	g.P(protoPkg.Ident("Message"))
	g.P(gc.marker, "()")
	g.P("}")
	g.P()
	for _, m := range gc.members {
		g.P("func (*", m.GoIdent.GoName, ") ", gc.marker, "() {}")
	}
	g.P()
	g.P("// ", gc.handlers, " handles each message type of channel ", gc.Key, ".")
	g.P("// A nil field acks that type unhandled. Unknown gets a type this code")
	g.P("// does not know; nil acks it unhandled.")
	g.P("type ", gc.handlers, " struct {")
	for _, m := range gc.members {
		g.P(m.GoIdent.GoName, " func(", ctxT, ", ", gc.subject, ", *", m.GoIdent.GoName, ") error")
	}
	g.P("Unknown func(", ctxT, ", ", gc.subject, ", *", msgT, ") error")
	g.P("}")
	g.P()
	g.P("// Publish", gc.goName, " publishes m on channel ", gc.Key, ".")
	g.P("func (b *Bus) Publish", gc.goName, "(ctx ", ctxT, ", s ", gc.subject, ", m ", gc.event, ", opts ...", pubOpt, ") error {")
	g.P("return b.core.Publish(ctx, ", gc.varName, ", s.segments(), m, opts...)")
	g.P("}")
	g.P()
	g.P("// On", gc.goName, " listens on channel ", gc.Key, " as group and calls the")
	g.P("// handler for each message's type. One consumer per (channel, group)")
	g.P("// carries every type. A nil error acks; an error redelivers.")
	g.P("func (b *Bus) On", gc.goName, "(ctx ", ctxT, ", group string, h ", gc.handlers, ", opts ...", subOpt, ") (stop func(), err error) {")
	g.P("return b.core.Subscribe(ctx, ", gc.varName, ", group, func(ctx ", ctxT, ", seg []string, msg *", msgT, ") error {")
	g.P("s := new", gc.subject, "(seg)")
	g.P("switch msg.Type() {")
	for _, m := range gc.members {
		g.P("case ", fmt.Sprintf("%q", m.Desc.FullName()), ":")
		g.P("if h.", m.GoIdent.GoName, " == nil {")
		g.P("return nil")
		g.P("}")
		g.P("m := new(", m.GoIdent.GoName, ")")
		g.P("if err := ", unmarshal, "(msg.Data, m); err != nil {")
		g.P("return err")
		g.P("}")
		g.P("return h.", m.GoIdent.GoName, "(ctx, s, m)")
	}
	g.P("}")
	g.P("if h.Unknown != nil {")
	g.P("return h.Unknown(ctx, s, msg)")
	g.P("}")
	g.P("b.core.Skip(", gc.varName, ", msg, \"no handler for this type\")")
	g.P("return nil")
	g.P("}, opts...)")
	g.P("}")
	g.P()
}

// goFieldType is the Go type of a singular (oneof) field's getter.
func goFieldType(g *protogen.GeneratedFile, f *protogen.Field) string {
	switch f.Desc.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "*" + g.QualifiedGoIdent(f.Message.GoIdent)
	case protoreflect.EnumKind:
		return g.QualifiedGoIdent(f.Enum.GoIdent)
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BytesKind:
		return "[]byte"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	}
	panic("eventgen: unhandled field kind " + f.Desc.Kind().String())
}

// goCamel turns a segment name into an exported Go field name:
// "orderId" -> "OrderId", "order_id" -> "OrderId".
func goCamel(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteRune(unicode.ToUpper(r))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "X"
	}
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
