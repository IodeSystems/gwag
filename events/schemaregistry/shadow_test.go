package schemaregistry_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

const corpMoney = `syntax = "proto3";
package corp.common.v1;
message Money { int64 cents = 1; string currency = 2; }
`

const corpOrders = `syntax = "proto3";
package corp.orders.v1;
import "corp/common/v1/money.proto";
message Order {
  string order_id = 1;
  corp.common.v1.Money total = 2;
  Status status = 3;
  repeated string skus = 4;
  map<string, int32> qty = 5;
  enum Status { STATUS_UNSPECIFIED = 0; PLACED = 1; SHIPPED = 2; }
}
`

func compile(t *testing.T, root string, files ...sr.TreeFile) protoreflect.FileDescriptor {
	t.Helper()
	fd, err := (&sr.Tree{Root: root, Files: files}).Compile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func corp(t *testing.T) protoreflect.FileDescriptor {
	return compile(t, "orders.proto", sr.TreeFile{Path: "corp/common/v1/money.proto", Source: corpMoney}, sr.TreeFile{Path: "orders.proto", Source: corpOrders})
}

func shadow(t *testing.T, body string) protoreflect.MessageDescriptor {
	fd := compile(t, "shadow.proto", sr.TreeFile{Path: "shadow.proto", Source: "syntax = \"proto3\";\npackage shop.v1;\n" + body})
	return fd.Messages().ByName("Order")
}

func problems(ps []sr.Problem) string {
	var out []string
	for _, p := range ps {
		s := p.String()
		if p.Warning {
			s = "warning " + s
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

func TestCompareConsumer(t *testing.T) {
	reg := corp(t).Messages().ByName("Order")
	for _, tc := range []struct {
		name, body, want string
	}{
		{"subset", `
message Money { int64 cents = 1; }
message Order { string order_id = 1; Money total = 2; S status = 3; map<string, int32> qty = 5;
  enum S { STATUS_UNSPECIFIED = 0; SHIPPED = 2; } }`, ""},
		{"renamed field warns", `message Order { string id = 1; }`, `warning id: registry names field 1 "order_id"`},
		{"type", `message Order { int32 order_id = 1; }`, "order_id: field 1 is int32, registry has string"},
		{"cardinality", `message Order { string skus = 4; }`, "skus: field 4 is singular, registry has repeated"},
		{"extra field", `message Order { string note = 9; }`, "note: field 9 is not in registry message corp.orders.v1.Order"},
		{"nested", `message Money { string cents = 1; } message Order { Money total = 2; }`, "total.cents: field 1 is string, registry has int64"},
		{"enum value", `message Order { S status = 3; enum S { STATUS_UNSPECIFIED = 0; LOST = 7; } }`,
			"status: enum value LOST = 7 is not in registry enum corp.orders.v1.Order.Status"},
		{"map value", `message Order { map<string, int64> qty = 5; }`, "qty<value>: field 2 is int64, registry has int32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := problems(sr.CompareConsumer(shadow(t, tc.body), reg)); got != tc.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// The candidate is the registry file with Order replaced by the shadow
// in the registry's package; fields the registry has keep its types.
func TestProducerCandidate(t *testing.T) {
	regFile := corp(t)
	md := shadow(t, `message Money { int64 cents = 1; }
message Order { string order_id = 1; Money total = 2; string note = 6; Kind kind = 7; enum Kind { A = 0; } }`)
	fdp, err := sr.ProducerCandidate(regFile, regFile.Messages().ByName("Order"), md)
	if err != nil {
		t.Fatal(err)
	}
	files := &protoregistry.Files{}
	_ = files.RegisterFile(regFile.Imports().Get(0).FileDescriptor)
	fd, err := protodesc.NewFile(fdp, files)
	if err != nil {
		t.Fatalf("candidate does not link: %v", err)
	}
	o := fd.Messages().ByName("Order")
	if o.FullName() != "corp.orders.v1.Order" || o.Fields().Len() != 4 {
		t.Fatalf("candidate %s has %d fields", o.FullName(), o.Fields().Len())
	}
	if got := o.Fields().ByNumber(2).Message().FullName(); got != "corp.common.v1.Money" {
		t.Fatalf("total is %s", got)
	}
	if got := o.Fields().ByNumber(7).Enum().FullName(); got != "corp.orders.v1.Order.Kind" {
		t.Fatalf("kind is %s", got)
	}
}
