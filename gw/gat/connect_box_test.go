package gat

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// boxDescs renders a Region message holding a nested list (PointsRow)
// and a map of maps (CellsValue), and a Plain message with neither.
func boxDescs(t *testing.T) (region, plain protoreflect.MessageDescriptor) {
	t.Helper()
	str := ir.TypeRef{Builtin: ir.ScalarString}
	i32 := ir.TypeRef{Builtin: ir.ScalarInt32}
	inner := ir.TypeRef{Map: &ir.MapType{KeyType: str, ValueType: i32}}
	svc := &ir.Service{
		Namespace: "t", Version: "v1", ServiceName: "Service",
		Types: map[string]*ir.Type{
			"Region": {Name: "Region", TypeKind: ir.TypeObject, Fields: []*ir.Field{
				{Name: "points", Type: ir.TypeRef{List: &ir.ListType{Elem: ir.TypeRef{Builtin: ir.ScalarDouble}}}, OneofIndex: -1},
				{Name: "cells", Type: ir.TypeRef{Map: &ir.MapType{KeyType: str, ValueType: inner}}, OneofIndex: -1},
			}},
			"Plain": {Name: "Plain", TypeKind: ir.TypeObject, Fields: []*ir.Field{
				{Name: "name", Type: str, OneofIndex: -1},
			}},
		},
	}
	fds, err := ir.RenderProtoFiles([]*ir.Service{svc})
	if err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatal(err)
	}
	files.RangeFiles(func(f protoreflect.FileDescriptor) bool {
		if m := f.Messages().ByName("Region"); m != nil {
			region = m
			plain = f.Messages().ByName("Plain")
		}
		return true
	})
	if region == nil || plain == nil {
		t.Fatal("rendered descriptors missing")
	}
	return region, plain
}

type boxPointRow struct {
	Value []float64 `json:"value"`
}
type boxCellsValue struct {
	Value map[string]int32 `json:"value"`
}
type boxRegion struct {
	Points []boxPointRow            `json:"points"`
	Cells  map[string]boxCellsValue `json:"cells"`
}
type boxPlain struct {
	Name string `json:"name"`
}

// A Go type shaped exactly like the rendered row and map-value boxes
// must still not get a fast plan: the slow path unboxes, the plans do
// not. Messages without boxes keep theirs.
func TestFastPlans_DeclineBoxes(t *testing.T) {
	region, plain := boxDescs(t)
	for _, name := range []string{"points", "cells"} {
		if !isBoxField(region.Fields().ByName(protoreflect.Name(name))) {
			t.Fatalf("%s is not recognised as a box field", name)
		}
	}
	if isBoxField(plain.Fields().Get(0)) {
		t.Fatal("plain string field flagged as a box")
	}

	rt := reflect.TypeFor[boxRegion]()
	if sb := buildStructBind(rt, region, 0); sb != nil {
		t.Error("buildStructBind formed a plan over boxes")
	}
	if plan := buildBindPlan(rt, irOp(bArg("points"), bArg("cells")), "", region); plan != nil {
		t.Error("buildBindPlan formed a plan over boxes")
	}
	if plan := buildEncodePlan(rt, region); plan != nil {
		t.Error("buildEncodePlan formed a plan over boxes")
	}
	if decodableMessage(region) {
		t.Error("decodableMessage accepted boxes")
	}

	pt := reflect.TypeFor[boxPlain]()
	if buildStructBind(pt, plain, 0) == nil {
		t.Error("buildStructBind declined a box-free message")
	}
	if buildEncodePlan(pt, plain) == nil {
		t.Error("buildEncodePlan declined a box-free message")
	}
	if !decodableMessage(plain) {
		t.Error("decodableMessage declined a box-free message")
	}
}
