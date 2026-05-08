package gateway

import (
	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// newProtoIRTypeBuilder builds a single *IRTypeBuilder shared across
// every proto pool. Proto FullNames are globally unique, so a merged
// Types map is collision-free; sharing the builder keeps cyclic
// references resolving to a single *graphql.Object across pools and
// across the Query / Subscription roots.
//
// Hides is applied to the merged service in-place so the type-builder
// emits Object/Input types without hidden fields.
func newProtoIRTypeBuilder(pools map[poolKey]*pool, hides map[string]bool) *IRTypeBuilder {
	merged := &ir.Service{Types: map[string]*ir.Type{}}
	for _, p := range pools {
		svcs := ir.IngestProto(p.file)
		for _, s := range svcs {
			for k, v := range s.Types {
				if _, ok := merged.Types[k]; !ok {
					merged.Types[k] = v
				}
			}
		}
	}
	if len(hides) > 0 {
		ir.Hides([]*ir.Service{merged}, hides)
	}
	return NewIRTypeBuilder(merged, IRTypeNaming{
		ObjectName: exportedName,
		EnumName:   exportedName,
		UnionName:  exportedName,
		InputName:  func(s string) string { return exportedName(s) + "_Input" },
		FieldName:  lowerCamel,
	}, IRTypeBuilderOptions{})
}
