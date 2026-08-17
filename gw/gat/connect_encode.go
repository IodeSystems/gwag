package gat

// Direct Go value → proto message encoding for the Connect/gRPC
// ingress response.
//
// The ingress reached the response message by text: json.Marshal the
// handler's output, then protojson.Unmarshal the bytes into a
// dynamicpb message. encodePlan walks the Go value straight into the
// message instead.
//
// The inbound direction (connect_args.go) could convert
// unconditionally because its source is a proto message — a closed set
// of kinds with fixed encodings. This direction cannot: the source is
// whatever Go type the adopter's handler returns, and json.Marshal
// honours json.Marshaler and encoding.TextMarshaler. A type like
// time.Time, json.RawMessage, or a custom enum with a MarshalJSON
// method serializes to something a field-by-field reflection walk
// would never produce, and the difference would be silent.
//
// So the plan is built once per operation at mount time and REFUSES to
// form whenever it meets anything it cannot prove it handles
// identically — a custom marshaler, an interface, a map, an
// unmappable field. A nil plan means the caller keeps using the JSON
// round trip. Correctness never depends on the analysis being clever;
// only speed does.

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// planResponse builds the encode plan for an operation's response, or
// returns nil to leave that operation on the JSON round trip.
//
// The Go type it plans against has to be the one extractBody actually
// hands back at dispatch time: the output struct's Body field when it
// has one, the output struct itself otherwise. Planning against the
// wrong type would produce a plan that silently writes nothing.
func planResponse(cap *capturedOp, md protoreflect.MethodDescriptor) *encodePlan {
	if cap == nil || cap.outputType == nil || md == nil {
		return nil
	}
	t := cap.outputType
	if t.Kind() == reflect.Struct {
		if f, ok := t.FieldByName("Body"); ok {
			t = f.Type
		}
	}
	t = derefType(t)
	if t == nil || t.Kind() != reflect.Struct {
		// A non-struct body (a bare slice or scalar) maps onto the
		// synthesized single-field response message in ways the JSON
		// path already gets right. Leave it there.
		return nil
	}
	return buildEncodePlan(t, md.Output())
}

// encodePlan describes how to write one Go type into one proto
// message. fields is ordered for stable iteration; order does not
// affect the result.
type encodePlan struct {
	fields []encodeField
}

// encodeField binds one proto field to the Go struct field that feeds
// it. index is a path so promoted fields from embedded structs work,
// matching how bindInput reaches parameters on the inbound side.
type encodeField struct {
	index []int
	fd    protoreflect.FieldDescriptor
	// sub is set for message-kinded fields: the plan for the nested
	// Go type. Built at mount time, so recursion cost is paid once.
	sub *encodePlan
}

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// buildEncodePlan pairs a Go type with a proto message descriptor.
// Returns nil when the pair cannot be proven equivalent to the JSON
// round trip, which is a normal outcome, not an error — the caller
// falls back.
func buildEncodePlan(t reflect.Type, md protoreflect.MessageDescriptor) *encodePlan {
	p, err := planStruct(t, md, 0)
	if err != nil {
		return nil
	}
	return p
}

// maxPlanDepth stops a self-referential Go type from recursing
// forever. Anything deeper falls back to the JSON path, which handles
// cycles by failing the same way json.Marshal always has.
const maxPlanDepth = 12

func planStruct(t reflect.Type, md protoreflect.MessageDescriptor, depth int) (*encodePlan, error) {
	if depth > maxPlanDepth {
		return nil, fmt.Errorf("plan too deep")
	}
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("not a struct: %s", t.Kind())
	}
	if hasCustomMarshaler(t) {
		return nil, fmt.Errorf("%s has a custom marshaler", t)
	}

	byName := map[string][]int{}
	collectJSONFields(t, nil, byName)

	plan := &encodePlan{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		// Proto field names are the IR arg / property names verbatim
		// (see synthRequestMessage in gw/ir/render_proto.go), which are
		// the JSON property names the handler's struct tags carry.
		idx, ok := byName[string(fd.Name())]
		if !ok {
			// A proto field with no Go source stays unset, exactly as
			// the JSON round trip would leave it.
			continue
		}
		ft := derefType(fieldTypeAt(t, idx))
		if hasCustomMarshaler(ft) {
			return nil, fmt.Errorf("field %s has a custom marshaler", fd.Name())
		}
		ef := encodeField{index: idx, fd: fd}

		elem := ft
		if fd.IsList() {
			if ft.Kind() != reflect.Slice && ft.Kind() != reflect.Array {
				return nil, fmt.Errorf("field %s: repeated but Go type is %s", fd.Name(), ft.Kind())
			}
			elem = derefType(ft.Elem())
			if hasCustomMarshaler(elem) {
				return nil, fmt.Errorf("field %s element has a custom marshaler", fd.Name())
			}
		} else if ft.Kind() == reflect.Slice && fd.Kind() != protoreflect.BytesKind {
			return nil, fmt.Errorf("field %s: Go slice for a singular proto field", fd.Name())
		}

		switch fd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if fd.IsMap() {
				// Proto maps arrive as MessageKind with a synthetic
				// entry type. Mapping a Go map through reflection
				// duplicates protojson's key-stringification rules for
				// little gain; leave these on the JSON path.
				return nil, fmt.Errorf("field %s is a map", fd.Name())
			}
			sub, err := planStruct(elem, fd.Message(), depth+1)
			if err != nil {
				return nil, err
			}
			ef.sub = sub
		default:
			if !scalarAssignable(fd.Kind(), elem.Kind()) {
				return nil, fmt.Errorf("field %s: %s not assignable from Go %s",
					fd.Name(), fd.Kind(), elem.Kind())
			}
		}
		plan.fields = append(plan.fields, ef)
	}
	if len(plan.fields) == 0 {
		// Nothing matched. That is not a fast path, it is a plan that
		// would emit an empty message — and the JSON round trip might
		// legitimately fill fields this walker failed to pair up. Refuse
		// and let the caller fall back.
		return nil, fmt.Errorf("no fields mapped between %s and %s", t, md.FullName())
	}
	return plan, nil
}

// scalarAssignable reports whether a Go kind can feed a proto kind
// without the numeric-widening or string-coercion games protojson
// plays. Deliberately strict: a mismatch refuses the plan rather than
// guessing.
func scalarAssignable(pk protoreflect.Kind, gk reflect.Kind) bool {
	switch pk {
	case protoreflect.BoolKind:
		return gk == reflect.Bool
	case protoreflect.StringKind:
		return gk == reflect.String
	case protoreflect.BytesKind:
		return gk == reflect.Slice
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		switch gk {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return true
		}
		return false
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		switch gk {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return true
		}
		return false
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return gk == reflect.Float32 || gk == reflect.Float64
	}
	// Enums and anything unlisted: the JSON path knows the encoding,
	// this walker does not.
	return false
}

// hasCustomMarshaler reports whether t (or *t) implements
// json.Marshaler or encoding.TextMarshaler. Either one means
// json.Marshal produces something a field walk cannot reproduce.
func hasCustomMarshaler(t reflect.Type) bool {
	if t == nil {
		return false
	}
	pt := reflect.PointerTo(t)
	for _, x := range []reflect.Type{t, pt} {
		if x.Implements(jsonMarshalerType) || x.Implements(textMarshalerType) {
			return true
		}
	}
	return false
}

// collectJSONFields maps JSON property name → field index path,
// descending exported embedded structs the way encoding/json promotes
// them. Fields tagged `json:"-"` are skipped, as json.Marshal skips
// them.
func collectJSONFields(t reflect.Type, prefix []int, out map[string][]int) {
	t = derefType(t)
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := append(append([]int{}, prefix...), i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && tag == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			// Embedded without an explicit name: encoding/json
			// promotes the inner fields. Descend.
			collectJSONFields(f.Type, path, out)
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, taken := out[name]; !taken {
			out[name] = path
		}
	}
}

func fieldTypeAt(t reflect.Type, index []int) reflect.Type {
	cur := derefType(t)
	for _, i := range index {
		cur = derefType(cur).Field(i).Type
	}
	return cur
}

func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// encode writes v into msg according to the plan. A nil or zero Go
// value leaves the proto field unset, matching what json.Marshal +
// protojson would do for an omitted or empty value under proto3
// presence rules.
func (p *encodePlan) encode(v reflect.Value, msg protoreflect.Message) error {
	v = derefValue(v)
	if !v.IsValid() {
		return nil
	}
	for _, ef := range p.fields {
		src, ok := fieldValueAt(v, ef.index)
		if !ok {
			continue
		}
		if err := setEncoded(msg, ef, src); err != nil {
			return err
		}
	}
	return nil
}

func setEncoded(msg protoreflect.Message, ef encodeField, src reflect.Value) error {
	src = derefValue(src)
	if !src.IsValid() {
		return nil
	}
	if ef.fd.IsList() {
		if src.Kind() != reflect.Slice && src.Kind() != reflect.Array {
			return fmt.Errorf("field %s: expected slice, got %s", ef.fd.Name(), src.Kind())
		}
		if src.Kind() == reflect.Slice && src.IsNil() {
			return nil
		}
		list := msg.Mutable(ef.fd).List()
		for i := 0; i < src.Len(); i++ {
			item := derefValue(src.Index(i))
			if ef.sub != nil {
				elem := list.NewElement()
				if err := ef.sub.encode(item, elem.Message()); err != nil {
					return err
				}
				list.Append(elem)
				continue
			}
			pv, err := goToProtoScalar(ef.fd, item)
			if err != nil {
				return fmt.Errorf("field %s[%d]: %w", ef.fd.Name(), i, err)
			}
			list.Append(pv)
		}
		return nil
	}

	if ef.sub != nil {
		sub := msg.Mutable(ef.fd).Message()
		return ef.sub.encode(src, sub)
	}

	// Proto3 scalars have no presence: a zero value is indistinguishable
	// from unset on the wire either way, so skipping the Set keeps the
	// message identical to what protojson produced from `omitempty`
	// output while avoiding pointless work.
	if src.IsZero() {
		return nil
	}
	pv, err := goToProtoScalar(ef.fd, src)
	if err != nil {
		return fmt.Errorf("field %s: %w", ef.fd.Name(), err)
	}
	msg.Set(ef.fd, pv)
	return nil
}

func goToProtoScalar(fd protoreflect.FieldDescriptor, v reflect.Value) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(v.Bool()), nil
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(v.String()), nil
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes(v.Bytes()), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(int32(v.Int())), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(v.Int()), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(uint32(v.Uint())), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(v.Uint()), nil
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(float32(v.Float())), nil
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(v.Float()), nil
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported kind %s", fd.Kind())
}

// fieldValueAt follows an index path, reporting false when a pointer
// on the way is nil — there is nothing to write in that case.
func fieldValueAt(v reflect.Value, index []int) (reflect.Value, bool) {
	cur := v
	for _, i := range index {
		cur = derefValue(cur)
		if !cur.IsValid() || cur.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		cur = cur.Field(i)
	}
	return cur, cur.IsValid()
}

func derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}
