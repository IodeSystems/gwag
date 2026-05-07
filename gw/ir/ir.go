// Package ir is the gateway's format-neutral schema intermediate
// representation.
//
// Three formats feed in (proto / OpenAPI / GraphQL introspection)
// and three formats render out. To avoid 6 direct converters and
// the duplication that comes with them, ingesters write into the
// IR and renderers read from it. Transforms (hide-fields, filter
// by namespace, multi-version-prefix, internal-namespace stripping)
// operate on the IR itself; format-specific code stays at the
// ingest / render edges.
//
// Round-trip semantics:
//   - Same-kind: ingest(proto)->ir->render(proto) preserves the
//     original FileDescriptorSet bytes (modulo source-code info).
//     Each entity's Origin slot holds the source descriptor /
//     openapi3.T / introspection node, and the same-kind renderer
//     uses it directly when present.
//   - Cross-kind: lossy. Renderers fall back to the canonical
//     fields (Args, Type, ProtoNumber if present, OpenAPI Format /
//     Pattern, etc.). Origin from a different kind is ignored.
//
// IR is the superset: every field a renderer might care about
// lives on the canonical structure, even if only one format
// natively expresses it (proto's field numbers, OpenAPI's regex
// patterns, GraphQL's directives). When in doubt, add it.
package ir

// Kind classifies an entity's source format. Used to decide whether
// the same-kind renderer's Origin shortcut applies.
type Kind int

const (
	KindUnset Kind = iota
	KindProto
	KindOpenAPI
	KindGraphQL
)

// Service is one (namespace, version) coordinate's worth of API
// surface — what `?service=ns:vN` filters on, what hides/internal
// transforms scope to, and what the gateway's pool / openapi-source
// / graphql-source structures correspond to one-for-one.
type Service struct {
	Namespace   string
	Version     string
	Description string

	// Internal namespaces (`_*`) are hidden from public renders by
	// default. Transforms.HideInternal drops them.
	Internal bool

	// ServiceName is the proto-level service identifier, e.g.
	// "GreeterService" — relevant when rendering back to proto so
	// methods nest under the right service. OpenAPI / GraphQL
	// renderers ignore it (their grouping is by namespace).
	ServiceName string

	Operations []*Operation
	// Types are keyed by their canonical full name. Proto uses
	// "<package>.<Name>"; OpenAPI uses the components/schemas key;
	// GraphQL uses the unprefixed type Name. Cross-kind renderers
	// rewrite refs as needed.
	Types map[string]*Type

	// OriginKind + Origin together let a same-kind renderer skip
	// the canonical→format projection and emit the source artifact
	// verbatim. Origin is one of:
	//   *descriptorpb.FileDescriptorProto  (KindProto)
	//   *openapi3.T                        (KindOpenAPI)
	//   *introspectionSchema slice or JSON (KindGraphQL)
	OriginKind Kind
	Origin     any
}

// OpKind groups operations by invocation shape so cross-kind
// renderers can pick their target field-root (Query / Mutation /
// Subscription) or HTTP verb without re-classifying every time.
type OpKind int

const (
	OpQuery        OpKind = iota // GET / unary read
	OpMutation                   // POST/PUT/PATCH/DELETE / unary write
	OpSubscription               // server-streaming
	// Bidi/client-streaming proto methods carry StreamingClient on
	// the Operation; non-proto renders filter them.
)

// Operation is one callable endpoint. Inputs are a flat list of
// args (proto's single input message → flattened; OpenAPI's
// path/query/body params → flat; GraphQL's args → flat). Output
// is a single type ref; subscription operations stream Output
// payloads.
type Operation struct {
	Name        string
	Kind        OpKind
	Description string
	Deprecated  string

	Args []*Arg

	// Output is the return type. Repeated/Required carry the
	// list/non-null wrapping of the result slot so renderers can
	// reconstruct e.g. GraphQL's `[User!]!` shape from the same
	// canonical fields used for Field slots.
	Output             *TypeRef // nil for "void" returns
	OutputRepeated     bool
	OutputRequired     bool
	OutputItemRequired bool // when Repeated, each element is non-null

	// Proto streaming flags. StreamingServer corresponds to
	// Kind=OpSubscription; StreamingClient is the rare case that
	// only proto can express, kept for round-trip fidelity.
	StreamingClient bool

	// HTTPMethod / HTTPPath are populated by the OpenAPI ingester
	// (the wire-level routing data) and used by the OpenAPI renderer
	// when present. Proto renderer derives a synthetic POST path
	// from /<ServiceFullName>/<Name>.
	HTTPMethod string
	HTTPPath   string
	Tags       []string

	OriginKind Kind
	Origin     any // *descriptorpb.MethodDescriptorProto / *openapi3.Operation / *introspectionField
}

// Arg is one input parameter. Proto operations distill their input
// message's fields into Args (so a renderer can flatten the
// request body into named GraphQL args / OpenAPI params); the
// proto renderer reverses this by re-synthesizing a request message
// from Args when the Origin isn't present.
type Arg struct {
	Name         string
	Type         TypeRef
	Repeated     bool
	Required     bool
	ItemRequired bool // when Repeated, each list element is non-null
	Description  string
	Default      any

	// OpenAPILocation reflects the OpenAPI parameter location for
	// args ingested from openapi (path / query / header / cookie /
	// body). Empty for other origins; OpenAPI renderer assumes
	// "body" when the arg's type is a complex object and "query"
	// for primitives.
	OpenAPILocation string
}

// TypeKind is the structural shape of a Type entry.
type TypeKind int

const (
	TypeObject TypeKind = iota // proto message / openapi object / graphql object
	TypeEnum
	TypeUnion     // graphql Union / openapi oneOf-with-discriminator
	TypeInterface // graphql Interface
	TypeScalar    // custom scalar; built-ins live in TypeRef.Builtin without a Type entry
	TypeInput     // graphql Input Object — separate from Object so the renderer keeps them apart
)

// Type is one named structural definition.
type Type struct {
	Name        string
	TypeKind    TypeKind
	Description string

	// Object/Input: ordered field list.
	Fields []*Field

	// Enum: values in declaration order.
	Enum []EnumValue

	// Union/Interface: names of concrete object types. Refs into
	// Service.Types.
	Variants []string

	OriginKind Kind
	// Origin is one of:
	//   *descriptorpb.DescriptorProto      (object, KindProto)
	//   *descriptorpb.EnumDescriptorProto  (enum, KindProto)
	//   *openapi3.SchemaRef                (KindOpenAPI)
	//   *introspectionType                 (KindGraphQL)
	Origin any
}

// EnumValue is one entry in TypeKind=Enum.
type EnumValue struct {
	Name        string
	Description string
	Number      int32 // proto enum value number; 0-based for non-proto origins
	Deprecated  string
}

// Field is one property on an Object/Input type. The same struct
// also represents one element of a oneof (with OneofIndex set).
type Field struct {
	Name         string
	JSONName     string // proto json_name override; equal to Name elsewhere
	Type         TypeRef
	Repeated     bool
	Required     bool
	ItemRequired bool // when Repeated, each list element is non-null
	Description  string
	Deprecated   string
	Default      any

	// Proto-specific:
	ProtoNumber int32 // ≥1 for proto-origin; 0 for synthesized fields
	OneofIndex  int32 // -1 if not in a oneof; otherwise the proto oneof index
	Optional    bool  // proto3 explicit `optional`

	// OpenAPI-specific schema constraints. Survive cross-kind only
	// where the target format can express them (e.g. min/max → no
	// proto equivalent; OpenAPI renderer carries them through).
	Format    string
	Pattern   string
	Minimum   *float64
	Maximum   *float64
	MinLength *int64
	MaxLength *int64
	Example   any
}

// TypeRef points at a primitive scalar, a named Type, or a map.
// Exactly one of Builtin / Named / Map is populated; Repeated is
// orthogonal and lives on Field.
type TypeRef struct {
	Builtin ScalarKind
	Named   string
	Map     *MapType
}

// MapType is proto's map<K,V> / OpenAPI's
// `type:object, additionalProperties:V`. GraphQL has no native
// map; renderers project to a JSON-shaped scalar or list of
// {key,value} pairs.
type MapType struct {
	KeyType   TypeRef
	ValueType TypeRef
}

// ScalarKind enumerates the primitive types every format speaks.
// Custom scalars (proto wrappers, GraphQL custom scalars, OpenAPI
// strings with rare formats) register as TypeKind=TypeScalar in
// Service.Types and TypeRef.Named points at them.
type ScalarKind int

const (
	ScalarUnknown ScalarKind = iota
	ScalarString
	ScalarBool
	ScalarInt32
	ScalarInt64
	ScalarUInt32
	ScalarUInt64
	ScalarFloat
	ScalarDouble
	ScalarBytes
	ScalarID        // GraphQL ID
	ScalarTimestamp // proto google.protobuf.Timestamp / openapi format=date-time
)

// IsBuiltin reports whether the ref points at a primitive scalar
// (no Service.Types lookup needed).
func (r TypeRef) IsBuiltin() bool { return r.Builtin != ScalarUnknown && r.Named == "" && r.Map == nil }

// IsMap reports whether this is a map ref.
func (r TypeRef) IsMap() bool { return r.Map != nil }

// IsNamed reports whether this is a ref into Service.Types.
func (r TypeRef) IsNamed() bool { return r.Named != "" }
