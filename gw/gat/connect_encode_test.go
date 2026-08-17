package gat

// encodePlan replaced a json.Marshal + protojson.Unmarshal round trip
// on the Connect response path. Same contract as the inbound walker:
// it is only safe if it produces the same message the round trip did.
//
// These tests keep jsonToDynamic as the oracle — encode both ways,
// require proto.Equal — and, just as importantly, pin the cases where
// the plan must REFUSE to form. A plan that quietly formed for a type
// carrying a custom MarshalJSON would emit a differently-shaped
// message with no error anywhere.

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type encProject struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Tags  []string `json:"tags,omitempty"`
	Count int32    `json:"count"`
	Ratio float64  `json:"ratio"`
	Flag  bool     `json:"flag"`
}

type encListBody struct {
	Projects []encProject `json:"projects"`
}

type encSingleBody struct {
	Project encProject `json:"project"`
}

// encTimestamped carries a time.Time — a json.Marshaler. The plan must
// refuse rather than write the struct's unexported fields.
type encTimestamped struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// encRaw carries a json.RawMessage, another custom marshaler.
type encRaw struct {
	ID   string          `json:"id"`
	Meta json.RawMessage `json:"meta"`
}

// encodeTestDescs builds Project / ListResp / SingleResp descriptors
// whose field names match the Go json tags, mirroring how
// synthRequestMessage names fields from IR properties verbatim.
func encodeTestDescs(t *testing.T) (list, single, marshaler protoreflect.MessageDescriptor) {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("enctest.proto"),
		Package: proto.String("enctest.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Project"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("tags", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, true),
					field("count", 4, descriptorpb.FieldDescriptorProto_TYPE_INT32, false),
					field("ratio", 5, descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, false),
					field("flag", 6, descriptorpb.FieldDescriptorProto_TYPE_BOOL, false),
				},
			},
			{
				Name: proto.String("ListResp"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("projects", 1, ".enctest.v1.Project", true),
				},
			},
			{
				Name: proto.String("SingleResp"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("project", 1, ".enctest.v1.Project", false),
				},
			},
			{
				// Field names match encTimestamped / encRaw, so the
				// marshaler-backed field is actually reached during
				// planning. A descriptor that didn't name it would let
				// the plan form for the wrong reason.
				Name: proto.String("MarshalerResp"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("created_at", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
					field("meta", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, false),
				},
			},
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{fdp},
	})
	if err != nil {
		t.Fatalf("NewFiles: %v", err)
	}
	find := func(name string) protoreflect.MessageDescriptor {
		d, err := files.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		return d.(protoreflect.MessageDescriptor)
	}
	return find("enctest.v1.ListResp"), find("enctest.v1.SingleResp"), find("enctest.v1.MarshalerResp")
}

// bothWays encodes body through the plan and through the JSON round
// trip, returning the two messages for comparison.
func bothWays(t *testing.T, body any, md protoreflect.MessageDescriptor) (planned, viaJSON *dynamicpb.Message) {
	t.Helper()
	plan := buildEncodePlan(derefType(reflect.TypeOf(body)), md)
	if plan == nil {
		t.Fatalf("expected a plan for %T", body)
	}
	planned = dynamicpb.NewMessage(md)
	if err := plan.encode(reflect.ValueOf(body), planned); err != nil {
		t.Fatalf("plan encode: %v", err)
	}
	viaJSON = dynamicpb.NewMessage(md)
	if err := jsonToDynamic(body, viaJSON); err != nil {
		t.Fatalf("jsonToDynamic: %v", err)
	}
	return planned, viaJSON
}

func TestEncodePlan_MatchesJSONRoundTrip_List(t *testing.T) {
	listMD, _, _ := encodeTestDescs(t)

	body := encListBody{Projects: []encProject{
		{ID: "p0", Name: "Alpha", Tags: []string{"core", "beta"}, Count: 3, Ratio: 0.25, Flag: true},
		{ID: "p1", Name: "Beta", Count: 0, Ratio: 0, Flag: false},
	}}

	planned, viaJSON := bothWays(t, body, listMD)
	if !proto.Equal(planned, viaJSON) {
		t.Errorf("plan diverges from JSON round trip\nplanned: %v\nviaJSON: %v", planned, viaJSON)
	}
}

func TestEncodePlan_MatchesJSONRoundTrip_Nested(t *testing.T) {
	_, singleMD, _ := encodeTestDescs(t)

	body := encSingleBody{Project: encProject{
		ID: "only", Name: "Solo", Tags: []string{"x"}, Count: 42, Ratio: 1.5, Flag: true,
	}}

	planned, viaJSON := bothWays(t, body, singleMD)
	if !proto.Equal(planned, viaJSON) {
		t.Errorf("plan diverges\nplanned: %v\nviaJSON: %v", planned, viaJSON)
	}
}

func TestEncodePlan_MatchesJSONRoundTrip_Empty(t *testing.T) {
	listMD, _, _ := encodeTestDescs(t)

	for name, body := range map[string]any{
		"nil slice":   encListBody{},
		"empty slice": encListBody{Projects: []encProject{}},
		"zero values": encListBody{Projects: []encProject{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			planned, viaJSON := bothWays(t, body, listMD)
			if !proto.Equal(planned, viaJSON) {
				t.Errorf("plan diverges\nplanned: %v\nviaJSON: %v", planned, viaJSON)
			}
		})
	}
}

// The refusal cases matter as much as the equality ones: a plan that
// formed here would silently emit a different message.
func TestEncodePlan_RefusesCustomMarshalers(t *testing.T) {
	_, _, marshalerMD := encodeTestDescs(t)

	cases := map[string]reflect.Type{
		"time.Time field":       reflect.TypeOf(encTimestamped{}),
		"json.RawMessage field": reflect.TypeOf(encRaw{}),
	}
	for name, typ := range cases {
		t.Run(name, func(t *testing.T) {
			if plan := buildEncodePlan(typ, marshalerMD); plan != nil {
				t.Errorf("expected no plan for %s — a custom marshaler must fall back to JSON", name)
			}
		})
	}
}

func TestEncodePlan_RefusesNonStruct(t *testing.T) {
	listMD, _, _ := encodeTestDescs(t)
	for name, typ := range map[string]reflect.Type{
		"slice":  reflect.TypeOf([]string{}),
		"string": reflect.TypeOf(""),
		"map":    reflect.TypeOf(map[string]any{}),
	} {
		t.Run(name, func(t *testing.T) {
			if plan := buildEncodePlan(typ, listMD); plan != nil {
				t.Errorf("expected no plan for %s", name)
			}
		})
	}
}

// The gatbench shapes are the ones the published numbers are measured
// on. If the plan silently stopped forming for them, the benchmark
// would still pass while quietly measuring the JSON path.
func TestEncodePlan_FormsForBenchmarkShapes(t *testing.T) {
	listMD, singleMD, _ := encodeTestDescs(t)

	if buildEncodePlan(reflect.TypeOf(encListBody{}), listMD) == nil {
		t.Error("no plan for the list body — the fast path is not being exercised")
	}
	if buildEncodePlan(reflect.TypeOf(encSingleBody{}), singleMD) == nil {
		t.Error("no plan for the single body — the fast path is not being exercised")
	}
}
