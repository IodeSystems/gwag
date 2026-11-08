package eventgen

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// pyReserved are the module-level names the generated file imports or
// defines itself. A proto message or generated name equal to one is a
// gen error, so the file never shadows anything.
var pyReserved = map[string]bool{
	"annotations": true, "Awaitable": true, "Callable": true, "Mapping": true, "Sequence": true,
	"Any": true, "TypeAlias": true, "cast": true, "dataclass": true, "betterproto2": true,
	"Backend": true, "Channel": true, "Core": true, "Message": true, "Stop": true,
	"SubscribeOptions": true, "Subscription": true, "message_from": true,
	"register_file_descriptors": true,
}

// emitPython writes <package dir>/<file>_events.py next to
// betterproto2's <package dir>/__init__.py for each .proto file that
// declares a channel.
func emitPython(in *Input) ([]*pluginpb.CodeGeneratorResponse_File, error) {
	if len(in.Channels) == 0 {
		return nil, nil
	}
	byFile := map[string][]*Channel{}
	for _, c := range in.Channels {
		byFile[c.File.Path()] = append(byFile[c.File.Path()], c)
	}
	var out []*pluginpb.CodeGeneratorResponse_File
	modules := map[string]string{}
	for _, f := range in.Files {
		chans := byFile[f.Path()]
		if len(chans) == 0 {
			continue
		}
		if f.Package() == "" {
			return nil, fmt.Errorf("%s: python event code needs a proto package", f.Path())
		}
		name := path.Join(strings.ReplaceAll(string(f.Package()), ".", "/"), pyModuleName(f.Path())+"_events.py")
		if prev, dup := modules[name]; dup {
			return nil, fmt.Errorf("%s and %s would both write %s; rename one file", prev, f.Path(), name)
		}
		modules[name] = f.Path()
		body, err := pyFile(f, chans, in)
		if err != nil {
			return nil, err
		}
		out = append(out, &pluginpb.CodeGeneratorResponse_File{Name: &name, Content: &body})
	}
	return out, nil
}

// pyModuleName is the file's base name as a module name.
func pyModuleName(protoPath string) string {
	b := strings.TrimSuffix(path.Base(protoPath), ".proto")
	var s strings.Builder
	for i := 0; i < len(b); i++ {
		if isAlnumASCII(b[i]) || b[i] == '_' {
			s.WriteByte(b[i])
		} else {
			s.WriteByte('_')
		}
	}
	r := s.String()
	if r == "" || isDigitASCII(r[0]) {
		r = "_" + r
	}
	return r
}

// pyClassName is the class betterproto2 gives a message or enum of the
// package: its name inside the package, pascal-cased.
func pyClassName(full protoreflect.FullName, pkg protoreflect.FullName) string {
	n := string(full)
	if pkg != "" {
		n = strings.TrimPrefix(n, string(pkg)+".")
	}
	return pyPascalCase(n)
}

// pyChannel is a channel with its Python names worked out.
type pyChannel struct {
	*Channel
	cls     string // OrderEvents: class-name stem, the channel name as written
	snake   string // order_events: method and function stem
	konst   string // _ORDER_EVENTS
	subject string // OrderEventsSubject
	event   string // OrderEventsEvent
	segs    []string
	cases   []pyCase // oneof cases or union members
	msgCls  string   // plain, oneof: payload class
}

// pyCase is one union member or oneof case: a handler keyword.
type pyCase struct {
	kw   string // handler keyword, snake case
	cls  string // payload class (union member, oneof message case) or ""
	typ  string // payload annotation
	full string // union: proto full name of the member
	attr string // oneof: betterproto2 field name
}

type pyFile_ struct {
	f      protoreflect.FileDescriptor
	chans  []*pyChannel
	bus    string
	imp    map[string]bool // classes imported from the package module
	header Header
}

func pyFile(f protoreflect.FileDescriptor, chans []*Channel, in *Input) (string, error) {
	pkg := f.Package()
	taken := map[string]string{}
	var walkMsgs func(ms protoreflect.MessageDescriptors)
	walkEnums := func(es protoreflect.EnumDescriptors) {
		for i := 0; i < es.Len(); i++ {
			taken[pyClassName(es.Get(i).FullName(), pkg)] = "enum " + string(es.Get(i).FullName())
		}
	}
	walkMsgs = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			taken[pyClassName(m.FullName(), pkg)] = "message " + string(m.FullName())
			walkEnums(m.Enums())
			walkMsgs(m.Messages())
		}
	}
	for _, g := range in.Files {
		if g.Package() == pkg {
			walkEnums(g.Enums())
			walkMsgs(g.Messages())
		}
	}
	for n, what := range taken {
		if pyReserved[n] {
			return "", fmt.Errorf("%s: %s collides with a name the generated file imports (%s); rename it", f.Path(), what, n)
		}
	}
	for n := range pyReserved {
		taken[n] = "a name the generated file imports"
	}
	claim := func(what, name string) error {
		if prev, ok := taken[name]; ok {
			return fmt.Errorf("%s: generated name %s (%s) collides with %s; rename the channel or message", f.Path(), name, what, prev)
		}
		taken[name] = what
		return nil
	}

	pf := &pyFile_{f: f, imp: map[string]bool{}, header: in.Header}
	pf.bus = pyPascalCase(pyModuleName(f.Path())) + "Bus"
	if err := claim("the bus of "+f.Path(), pf.bus); err != nil {
		return "", err
	}
	methods := map[string]string{"core": "the core property", "subscriptions": "the subscriptions method"}
	method := func(c *Channel, name string) error {
		if prev, ok := methods[name]; ok {
			return fmt.Errorf("channel %s: method %s collides with %s; rename the channel", c.Key, name, prev)
		}
		methods[name] = "channel " + c.Key
		return nil
	}

	for _, c := range chans {
		pc := &pyChannel{Channel: c, cls: upperFirst(c.Name)}
		pc.snake = strings.ToLower(pySnakeCase(c.Name))
		pc.konst = "_" + strings.ToUpper(pc.snake)
		pc.subject = pc.cls + "Subject"
		pc.event = pc.cls + "Event"
		names := []string{pc.subject}
		priv := []string{pc.konst, "_" + pc.snake + "_segments", "_" + pc.snake + "_subject", "_" + pc.snake + "_types"}
		for _, n := range priv {
			if err := claim("channel "+c.Key, n); err != nil {
				return "", err
			}
		}
		seen := map[string]bool{}
		for _, s := range c.Pattern.Segments() {
			var n string
			switch s.Kind {
			case events.SegmentNamed:
				n = pySafeSnakeCase(s.Name)
			default:
				n = strings.ToLower(s.Name) // P1 -> p1, Rest -> rest
			}
			if seen[n] {
				return "", fmt.Errorf("channel %s: segments name the same Python field %s", c.Key, n)
			}
			seen[n] = true
			pc.segs = append(pc.segs, n)
		}
		for _, n := range []string{"publish_" + pc.snake, "on_" + pc.snake} {
			if err := method(c, n); err != nil {
				return "", err
			}
		}
		kws := map[string]bool{"group": true, "options": true, "unknown": true, "self": true}
		kw := func(n string) error {
			if kws[n] {
				return fmt.Errorf("channel %s: handler keyword %q is taken; the message or case is named like a parameter", c.Key, n)
			}
			kws[n] = true
			return nil
		}
		switch c.Kind {
		case KindPlain:
			pc.msgCls = pyClassName(c.Message.FullName(), pkg)
			pf.imp[pc.msgCls] = true
		case KindOneof:
			pc.msgCls = pyClassName(c.Message.FullName(), pkg)
			pf.imp[pc.msgCls] = true
			if err := method(c, "on_"+pc.snake+"_message"); err != nil {
				return "", err
			}
			fields := c.Message.Fields()
			for i := 0; i < fields.Len(); i++ {
				fd := fields.Get(i)
				attr := pySafeSnakeCase(string(fd.Name()))
				if err := kw(attr); err != nil {
					return "", err
				}
				cs := pyCase{kw: attr, attr: attr}
				cs.typ, cs.cls = pyFieldType(fd, pkg)
				if cs.cls != "" {
					pf.imp[cs.cls] = true
				}
				pc.cases = append(pc.cases, cs)
			}
		case KindUnion:
			names = append(names, pc.event)
			for _, md := range c.Members {
				cls := pyClassName(md.FullName(), pkg)
				pf.imp[cls] = true
				k := strings.ToLower(pySnakeCase(cls))
				if err := kw(k); err != nil {
					return "", err
				}
				pc.cases = append(pc.cases, pyCase{kw: k, cls: cls, typ: cls, full: string(md.FullName())})
			}
		}
		for _, n := range names {
			if err := claim("channel "+c.Key, n); err != nil {
				return "", err
			}
		}
		pf.chans = append(pf.chans, pc)
	}
	return pf.render(), nil
}

// pyFieldType returns the annotation of a oneof case's value and,
// when it is a class of this package, the class to import. Types from
// other packages (and the well-known types betterproto2 unwraps) are
// Any.
func pyFieldType(fd protoreflect.FieldDescriptor, pkg protoreflect.FullName) (typ, cls string) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if fd.Message().ParentFile().Package() == pkg {
			c := pyClassName(fd.Message().FullName(), pkg)
			return c, c
		}
		return "Any", ""
	case protoreflect.EnumKind:
		if fd.Enum().ParentFile().Package() == pkg {
			c := pyClassName(fd.Enum().FullName(), pkg)
			return c, c
		}
		return "Any", ""
	case protoreflect.BoolKind:
		return "bool", ""
	case protoreflect.StringKind:
		return "str", ""
	case protoreflect.BytesKind:
		return "bytes", ""
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "float", ""
	}
	return "int", ""
}

type pyWriter struct{ b strings.Builder }

func (w *pyWriter) P(parts ...string) {
	w.b.WriteString(strings.Join(parts, ""))
	w.b.WriteByte('\n')
}

func pyQuote(s string) string { return strconv.Quote(s) }

func (pf *pyFile_) render() string {
	w := &pyWriter{}
	w.P("# Code generated by gwag gen. DO NOT EDIT.")
	w.P("# versions:")
	for _, l := range pf.header.Lines() {
		w.P("#   ", l)
	}
	w.P("# source: ", pf.f.Path())
	w.P()
	w.P("from __future__ import annotations")
	w.P()
	needAny, needCast, needBP := false, false, false
	for _, c := range pf.chans {
		if c.Kind == KindOneof {
			needBP = true
			for _, cs := range c.cases {
				if cs.typ == "Any" {
					needAny = true
				} else {
					needCast = true
				}
			}
		}
	}
	w.P("from collections.abc import Awaitable, Callable, Mapping, Sequence")
	w.P("from dataclasses import dataclass")
	var typing []string
	if needAny {
		typing = append(typing, "Any")
	}
	if needCast {
		typing = append(typing, "cast")
	}
	if pf.hasUnion() {
		typing = append(typing, "TypeAlias")
	}
	if len(typing) > 0 {
		sort.Strings(typing)
		w.P("from typing import ", strings.Join(typing, ", "))
	}
	w.P()
	if needBP {
		w.P("import betterproto2")
		w.P("from gwag_events import (")
	} else {
		w.P("from gwag_events import (")
	}
	for _, n := range []string{"Backend", "Channel", "Core", "Message", "Stop", "SubscribeOptions", "Subscription", "message_from", "register_file_descriptors"} {
		if n == "message_from" && !needBP {
			continue
		}
		w.P("    ", n, ",")
	}
	w.P(")")
	w.P()
	classes := make([]string, 0, len(pf.imp))
	for c := range pf.imp {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	w.P("from . import (")
	for _, c := range classes {
		w.P("    ", c, ",")
	}
	w.P(")")
	w.P()
	exports := []string{pf.bus}
	for _, c := range pf.chans {
		exports = append(exports, c.subject)
		if c.Kind == KindUnion {
			exports = append(exports, c.event)
		}
	}
	w.P("__all__ = [")
	for _, e := range exports {
		w.P("    ", pyQuote(e), ",")
	}
	w.P("]")
	pf.descriptors(w)
	for _, c := range pf.chans {
		pf.channel(w, c)
	}
	pf.bus_(w)
	return w.b.String()
}

// descriptors registers the serialized FileDescriptorProto of the file
// and of each import a Schema Registry would hold as a reference,
// imports first. betterproto2 messages carry no descriptor; the
// Schema Registry Serde (gwag-events-schemaregistry) needs them to
// register a schema and to find a message's index in its file. They
// are the bytes the Go runtime registers, so both languages produce
// the same schema.
func (pf *pyFile_) descriptors(w *pyWriter) {
	var files [][2]string
	seen := map[string]bool{}
	var visit func(fd protoreflect.FileDescriptor)
	visit = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] || schemaregistry.Builtin(fd.Path()) {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			visit(imps.Get(i).FileDescriptor)
		}
		s, err := schemaregistry.SchemaOf(fd, nil)
		if err != nil {
			// SchemaOf marshals a descriptor the compiler built; it
			// cannot fail on a valid one.
			panic(err)
		}
		files = append(files, [2]string{fd.Path(), s.Schema})
	}
	visit(pf.f)
	w.P()
	w.P()
	w.P("# Serialized FileDescriptorProtos for the Schema Registry serde.")
	w.P("register_file_descriptors(")
	w.P("    {")
	for _, f := range files {
		w.P("        ", pyQuote(f[0]), ": (")
		for b := f[1]; len(b) > 0; {
			n := min(len(b), 96)
			w.P("            ", pyQuote(b[:n]))
			b = b[n:]
		}
		w.P("        ),")
	}
	w.P("    }")
	w.P(")")
}

func (pf *pyFile_) hasUnion() bool {
	for _, c := range pf.chans {
		if c.Kind == KindUnion {
			return true
		}
	}
	return false
}

func (pf *pyFile_) channel(w *pyWriter, c *pyChannel) {
	pat := c.Pattern.String()
	w.P()
	w.P()
	w.P("# Channel ", c.Key, ", pattern ", pyQuote(pat), ".")
	w.P(c.konst, " = Channel.new(", pyQuote(c.Key), ", ", pyQuote(pat), ")")
	w.P()
	w.P()
	w.P("@dataclass(frozen=True, slots=True)")
	w.P("class ", c.subject, ":")
	w.P("    \"\"\"Wildcard segments of a subject on channel ", c.Key, " (pattern ", pyQuote(pat), ").\"\"\"")
	if len(c.segs) == 0 {
		w.P()
		w.P("    pass")
	} else {
		w.P()
		for _, s := range c.segs {
			w.P("    ", s, ": str")
		}
	}
	w.P()
	w.P()
	w.P("def _", c.snake, "_segments(s: ", c.subject, ") -> list[str]:")
	if len(c.segs) == 0 {
		w.P("    return []")
	} else {
		parts := make([]string, len(c.segs))
		for i, s := range c.segs {
			parts[i] = "s." + s
		}
		w.P("    return [", strings.Join(parts, ", "), "]")
	}
	w.P()
	w.P()
	w.P("def _", c.snake, "_subject(v: Sequence[str]) -> ", c.subject, ":")
	if len(c.segs) == 0 {
		w.P("    return ", c.subject, "()")
	} else {
		w.P("    return ", c.subject, "(")
		for i, s := range c.segs {
			w.P("        ", s, "=v[", strconv.Itoa(i), "],")
		}
		w.P("    )")
	}
	if c.Kind == KindUnion {
		names := make([]string, len(c.cases))
		for i, cs := range c.cases {
			names[i] = cs.cls
		}
		w.P()
		w.P()
		w.P(c.event, ": TypeAlias = ", strings.Join(names, " | "))
		w.P("\"\"\"A message channel ", c.Key, " carries.\"\"\"")
		w.P()
		w.P()
		w.P("_", c.snake, "_types: tuple[tuple[type, str], ...] = (")
		for _, cs := range c.cases {
			w.P("    (", cs.cls, ", ", pyQuote(cs.full), "),")
		}
		w.P(")")
		w.P()
		w.P()
		w.P("def _", c.snake, "_type_name(m: object) -> str:")
		w.P("    for cls, name in _", c.snake, "_types:")
		w.P("        if isinstance(m, cls):")
		w.P("            return name")
		w.P("    raise TypeError(", "f\"channel ", c.Key, " does not carry {type(m).__name__}\")")
	}
}

func (pf *pyFile_) bus_(w *pyWriter) {
	w.P()
	w.P()
	w.P("class ", pf.bus, ":")
	w.P("    \"\"\"Publishes and listens on the event channels declared in")
	w.P("    ", pf.f.Path(), ".\"\"\"")
	w.P()
	w.P("    def __init__(self, backend: Backend, *, logf: Callable[[str], None] | None = None) -> None:")
	w.P("        self._core = Core(backend, logf=logf)")
	w.P()
	w.P("    @property")
	w.P("    def core(self) -> Core:")
	w.P("        return self._core")
	w.P()
	w.P("    def subscriptions(self) -> list[Subscription]:")
	w.P("        \"\"\"The (channel, group) pairs this bus listens on.\"\"\"")
	w.P("        return self._core.subscriptions()")
	for _, c := range pf.chans {
		switch c.Kind {
		case KindPlain:
			pf.publish(w, c, c.msgCls, pyQuote(string(c.Message.FullName())))
			pf.onWhole(w, c, "on_"+c.snake, false)
		case KindOneof:
			pf.publish(w, c, c.msgCls, pyQuote(string(c.Message.FullName())))
			pf.onWhole(w, c, "on_"+c.snake+"_message", true)
			pf.onOneof(w, c)
		case KindUnion:
			pf.publish(w, c, c.event, "_"+c.snake+"_type_name(message)")
			pf.onUnion(w, c)
		}
	}
}

func (pf *pyFile_) publish(w *pyWriter, c *pyChannel, typ, typeName string) {
	w.P()
	w.P("    async def publish_", c.snake, "(")
	w.P("        self,")
	w.P("        subject: ", c.subject, ",")
	w.P("        message: ", typ, ",")
	w.P("        *,")
	w.P("        msg_id: str | None = None,")
	w.P("        headers: Mapping[str, str] | None = None,")
	w.P("    ) -> None:")
	w.P("        \"\"\"Publish message on channel ", c.Key, ".\"\"\"")
	w.P("        await self._core.publish(")
	w.P("            ", c.konst, ", _", c.snake, "_segments(subject), message, ", typeName, ", msg_id=msg_id, headers=headers")
	w.P("        )")
}

// onWhole writes the handler-takes-the-message form: the plain
// channel's on_x, and the oneof channel's on_x_message.
func (pf *pyFile_) onWhole(w *pyWriter, c *pyChannel, name string, oneof bool) {
	fqn := string(c.Message.FullName())
	w.P()
	w.P("    async def ", name, "(")
	w.P("        self,")
	w.P("        group: str,")
	w.P("        handler: Callable[[", c.subject, ", ", c.msgCls, "], Awaitable[None]],")
	w.P("        *,")
	w.P("        options: SubscribeOptions | None = None,")
	w.P("    ) -> Stop:")
	w.P("        \"\"\"Listen on channel ", c.Key, " as group. Subscribers sharing a group")
	w.P("        share its messages. Returning from handler acks the message; raising")
	w.P("        redelivers it.\"\"\"")
	w.P()
	w.P("        async def deliver(seg: list[str], msg: Message) -> None:")
	w.P("            if msg.type and msg.type != ", pyQuote(fqn), ":")
	w.P("                self._core.skip(", c.konst, ", msg, ", pyQuote("not "+fqn), ")")
	w.P("                return")
	w.P("            await handler(_", c.snake, "_subject(seg), ", c.msgCls, ".parse(msg.data))")
	w.P()
	w.P("        return await self._core.subscribe(", c.konst, ", group, deliver, options)")
}

func (pf *pyFile_) handlerKwargs(w *pyWriter, c *pyChannel, unknownType string) {
	for _, cs := range c.cases {
		w.P("        ", cs.kw, ": Callable[[", c.subject, ", ", cs.typ, "], Awaitable[None]] | None = None,")
	}
	w.P("        unknown: Callable[[", c.subject, ", ", unknownType, "], Awaitable[None]] | None = None,")
	w.P("        options: SubscribeOptions | None = None,")
}

func (pf *pyFile_) onOneof(w *pyWriter, c *pyChannel) {
	oneof := string(c.Oneof.Name())
	w.P()
	w.P("    async def on_", c.snake, "(")
	w.P("        self,")
	w.P("        group: str,")
	w.P("        *,")
	pf.handlerKwargs(w, c, c.msgCls)
	w.P("    ) -> Stop:")
	w.P("        \"\"\"Listen on channel ", c.Key, " as group and call the handler for the")
	w.P("        message's ", oneof, " case. A case without a handler is acked. unknown gets")
	w.P("        a message with no case set, or one this code does not know.\"\"\"")
	w.P()
	w.P("        async def deliver(s: ", c.subject, ", m: ", c.msgCls, ") -> None:")
	w.P("            which, value = betterproto2.which_one_of(m, ", pyQuote(oneof), ")")
	for _, cs := range c.cases {
		w.P("            if which == ", pyQuote(cs.attr), ":")
		w.P("                if ", cs.kw, " is not None:")
		if cs.typ == "Any" {
			w.P("                    await ", cs.kw, "(s, value)")
		} else {
			w.P("                    await ", cs.kw, "(s, cast(", cs.typ, ", value))")
		}
		w.P("                return")
	}
	w.P("            if unknown is not None:")
	w.P("                await unknown(s, m)")
	w.P("                return")
	w.P("            msg = message_from()")
	w.P("            if msg is not None:")
	w.P("                self._core.skip(", c.konst, ", msg, \"no case set, or no handler for it\")")
	w.P()
	w.P("        return await self.on_", c.snake, "_message(group, deliver, options=options)")
}

func (pf *pyFile_) onUnion(w *pyWriter, c *pyChannel) {
	w.P()
	w.P("    async def on_", c.snake, "(")
	w.P("        self,")
	w.P("        group: str,")
	w.P("        *,")
	pf.handlerKwargs(w, c, "Message")
	w.P("    ) -> Stop:")
	w.P("        \"\"\"Listen on channel ", c.Key, " as group and call the handler for each")
	w.P("        message's type. One consumer per (channel, group) carries every type.")
	w.P("        A type without a handler is acked. unknown gets a type this code does")
	w.P("        not know, as the raw message.\"\"\"")
	w.P()
	w.P("        async def deliver(seg: list[str], msg: Message) -> None:")
	w.P("            s = _", c.snake, "_subject(seg)")
	for _, cs := range c.cases {
		w.P("            if msg.type == ", pyQuote(cs.full), ":")
		w.P("                if ", cs.kw, " is not None:")
		w.P("                    await ", cs.kw, "(s, ", cs.cls, ".parse(msg.data))")
		w.P("                return")
	}
	w.P("            if unknown is not None:")
	w.P("                await unknown(s, msg)")
	w.P("                return")
	w.P("            self._core.skip(", c.konst, ", msg, \"no handler for this type\")")
	w.P()
	w.P("        return await self._core.subscribe(", c.konst, ", group, deliver, options)")
}
