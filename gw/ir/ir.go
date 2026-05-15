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
//
// Stability: stable
type Kind int

// Kind constants for source format classification.
//
// Stability: stable
const (
	KindUnset Kind = iota
	KindProto
	KindOpenAPI
	KindGraphQL
	KindMCP
)

// Service is one (namespace, version) coordinate's worth of API
// surface — what `?service=ns:vN` filters on, what hides/internal
// transforms scope to, and what the gateway's pool / openapi-source
// / graphql-source structures correspond to one-for-one.
//
// Stability: stable
type Service struct {
	Namespace   string
	Version     string
	Description string

	// Internal namespaces (`_*`) are hidden from public renders by
	// default. Transforms.HideInternal drops them.
	Internal bool

	// Deprecated, when non-empty, stamps `@deprecated(reason: ...)`
	// on every field projecting from this service. OR-combined with
	// the renderer's auto-deprecation of older `vN` cuts — either
	// trigger lights up the directive. Set by manual operator action
	// (cp.Deprecate); the renderer doesn't write here.
	Deprecated string

	// ServiceName is the proto-level service identifier, e.g.
	// "GreeterService" — relevant when rendering back to proto so
	// methods nest under the right service. OpenAPI / GraphQL
	// renderers ignore it (their grouping is by namespace).
	ServiceName string

	// Operations directly under this service's root. proto/OpenAPI
	// ingest leave Groups empty and put every method here. GraphQL
	// ingest classifies each root field: bare operations land here,
	// fields whose return type is a "namespace-shaped" object move
	// into Groups (see OperationGroup).
	Operations []*Operation

	// Groups model GraphQL's "object-type-as-namespace" pattern —
	// recursive sub-namespaces with their own operations and further
	// nested groups. Empty for proto / OpenAPI ingest. The GraphQL
	// renderer emits each Group as a synthesized Object type;
	// proto / OpenAPI renderers walk the tree depth-first and join
	// path segments with `_` so the flat method names round-trip.
	Groups []*OperationGroup

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

	// ChannelBindings declares pub/sub channel→payload-type pairs the
	// service ships alongside its operations. Proto ingest fills this
	// from the `(gwag.ps.binding)` custom message option at slot bake;
	// the runtime `WithChannelBinding` API fills the same slot for non-
	// proto adopters. The gateway aggregates across every registered
	// slot into a process-wide pattern→FQN lookup; on Event delivery
	// the matched FQN is stamped onto `Event.payload_type` so
	// subscribers can fetch the descriptor and decode. Format-neutral
	// by design — OpenAPI / GraphQL renderers ignore it, but the field
	// rides through transforms and schema rebuild like any other.
	ChannelBindings []ChannelBinding
}

// ChannelBinding is one proto-declarative or runtime-declared pub/sub
// channel→message-type pairing. Pattern uses NATS-style wildcards
// (segments split on `.`, `*` matches one segment, `>` matches the
// rest), identical to the grammar `WithChannelAuth` uses. MessageFQN
// is the proto fully-qualified message name (`"<package>.<Name>"`,
// matching the keys in Service.Types for proto-origin services).
//
// Stability: stable
type ChannelBinding struct {
	Pattern    string
	MessageFQN string
}

// OperationGroup is one nested namespace under a Service (or under
// another Group). Used for GraphQL's object-type-as-namespace
// pattern: in SDL terms, a Group is the field on the parent Object
// whose return type is itself an Object containing operations.
//
// proto / OpenAPI have no native equivalent — their renderers
// flatten by joining the path with `_`. The graphql renderer emits
// a synthesized container type per group, named `<parentName><Name>`
// pascal-cased (e.g. greeter → GreeterNamespace, then v1 →
// GreeterV1Namespace under it).
//
// Kind binds the group to one of GraphQL's three root operation
// types — every Operation inside (transitively) shares this Kind
// since a single GraphQL field on Query can't host a Mutation. A
// namespace that needs both queries and mutations (e.g. admin)
// emits as two sibling Groups under Service: one with Kind=OpQuery,
// one with Kind=OpMutation.
//
// Stability: stable
type OperationGroup struct {
	Name        string
	Description string
	Kind        OpKind
	Operations  []*Operation
	Groups      []*OperationGroup // recursive

	// OriginKind tags the registration kind that produced this group.
	// Only GraphQL ingest creates Groups today (`tryGraphQLGroup`); proto
	// and OpenAPI flatten via `_` path-joining. The runtime renderer
	// consults this to install a per-group forwarding resolver when the
	// upstream is GraphQL: one round trip per group selection, with
	// nested fields dereferenced from the upstream response by
	// graphql-go's DefaultResolveFn.
	OriginKind Kind

	// SchemaID is the dispatcher-registry key for this group's
	// forwarding resolver. Populated by `PopulateSchemaIDs` with a
	// `_group_<path>` suffix so it doesn't collide with leaf Operation
	// SchemaIDs.
	SchemaID SchemaID
}

// OpKind groups operations by invocation shape so cross-kind
// renderers can pick their target field-root (Query / Mutation /
// Subscription) or HTTP verb without re-classifying every time.
//
// Stability: stable
type OpKind int

// OpKind constants for operation classification.
//
// Stability: stable
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
//
// Stability: stable
type Operation struct {
	Name        string
	Kind        OpKind
	Description string
	Deprecated  string

	// SchemaID is the registry key for the runtime Dispatcher that
	// serves this operation. Empty until PopulateSchemaIDs is called
	// (which the gateway does once Namespace/Version are assigned to
	// the containing Service). Renderers that build runtime resolvers
	// look the Dispatcher up by this id; transforms preserve it.
	SchemaID SchemaID

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

	// MultipartBody is true for OpenAPI operations whose requestBody
	// content type is multipart/form-data. The schema's properties are
	// flattened into top-level Args (each with
	// OpenAPILocation="formdata"); binary properties land as
	// TypeRef{Builtin: ScalarUpload}. Dispatchers and ingress decoders
	// branch on this flag to build / parse multipart bodies instead of
	// JSON.
	MultipartBody bool

	OriginKind Kind
	Origin     any // *descriptorpb.MethodDescriptorProto / *openapi3.Operation / *introspectionField
}

// Arg is one input parameter. Proto operations distill their input
// message's fields into Args (so a renderer can flatten the
// request body into named GraphQL args / OpenAPI params); the
// proto renderer reverses this by re-synthesizing a request message
// from Args when the Origin isn't present.
//
// Stability: stable
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
//
// Stability: stable
type TypeKind int

// TypeKind constants for the structural shape of a Type entry.
//
// Stability: stable
const (
	TypeObject TypeKind = iota // proto message / openapi object / graphql object
	TypeEnum
	TypeUnion     // graphql Union / openapi oneOf-with-discriminator
	TypeInterface // graphql Interface
	TypeScalar    // custom scalar; built-ins live in TypeRef.Builtin without a Type entry
	TypeInput     // graphql Input Object — separate from Object so the renderer keeps them apart
)

// Type is one named structural definition.
//
// Stability: stable
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

	// DiscriminatorProperty (Union types only) names a field on the
	// runtime value that carries the variant tag — OpenAPI's
	// `schema.discriminator.propertyName`. Renderers building runtime
	// type-resolvers (e.g. IRTypeBuilder.unionFor) consult this when
	// non-empty; empty means fall through to the format-native
	// convention (e.g. GraphQL's `__typename`).
	DiscriminatorProperty string

	// DiscriminatorMapping maps wire-level discriminator values to
	// variant Type names. OpenAPI populates this from
	// `schema.discriminator.mapping` (with $ref leaves stripped to
	// the bare schema name); GraphQL ingest leaves it nil. Renderers
	// fall back to a "discriminator-value == variant-name" identity
	// match when a value isn't in the mapping.
	DiscriminatorMapping map[string]string

	OriginKind Kind
	// Origin is one of:
	//   *descriptorpb.DescriptorProto      (object, KindProto)
	//   *descriptorpb.EnumDescriptorProto  (enum, KindProto)
	//   *openapi3.SchemaRef                (KindOpenAPI)
	//   *introspectionType                 (KindGraphQL)
	Origin any
}

// EnumValue is one entry in TypeKind=Enum.
//
// Stability: stable
type EnumValue struct {
	Name        string
	Description string
	Number      int32 // proto enum value number; 0-based for non-proto origins
	Deprecated  string
}

// Field is one property on an Object/Input type. The same struct
// also represents one element of a oneof (with OneofIndex set).
//
// Stability: stable
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
//
// Stability: stable
type TypeRef struct {
	Builtin ScalarKind
	Named   string
	Map     *MapType
}

// MapType is proto's map<K,V> / OpenAPI's
// `type:object, additionalProperties:V`. GraphQL has no native
// map; renderers project to a JSON-shaped scalar or list of
// {key,value} pairs.
//
// Stability: stable
type MapType struct {
	KeyType   TypeRef
	ValueType TypeRef
}

// ScalarKind enumerates the primitive types every format speaks.
// Custom scalars (proto wrappers, GraphQL custom scalars, OpenAPI
// strings with rare formats) register as TypeKind=TypeScalar in
// Service.Types and TypeRef.Named points at them.
//
// Stability: stable
type ScalarKind int

// ScalarKind constants for primitive types shared across all formats.
//
// Stability: stable
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
	ScalarUpload    // file upload; OpenAPI string/format:binary in multipart/form-data
)

// FlatOperations returns Service.Operations plus every operation
// transitively contained in Service.Groups, with each grouped op's
// Name path-joined by "_" (e.g. group "greeter" → group "v1" → op
// "hello" renders as "greeter_v1_hello"). Renderers that don't
// natively model nested namespaces (proto, OpenAPI) call this so
// the flat method-name space the source format requires is
// derived deterministically from the IR tree.
//
// Operations from Groups are deep-copied; top-level Operations are
// returned by reference. Callers that mutate the slice should not
// rely on the top-level pointers being unique.
//
// Stability: stable
func (s *Service) FlatOperations() []*Operation {
	out := append([]*Operation(nil), s.Operations...)
	for _, g := range s.Groups {
		flattenGroupOps(g, "", &out)
	}
	return out
}

func flattenGroupOps(g *OperationGroup, prefix string, out *[]*Operation) {
	pre := prefix + g.Name + "_"
	for _, op := range g.Operations {
		clone := *op
		clone.Name = pre + op.Name
		*out = append(*out, &clone)
	}
	for _, sub := range g.Groups {
		flattenGroupOps(sub, pre, out)
	}
}

// IsBuiltin reports whether the ref points at a primitive scalar
// (no Service.Types lookup needed).
//
// Stability: stable
func (r TypeRef) IsBuiltin() bool { return r.Builtin != ScalarUnknown && r.Named == "" && r.Map == nil }

// IsMap reports whether this is a map ref.
//
// Stability: stable
func (r TypeRef) IsMap() bool { return r.Map != nil }

// IsNamed reports whether this is a ref into Service.Types.
//
// Stability: stable
func (r TypeRef) IsNamed() bool { return r.Named != "" }
