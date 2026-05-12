package gateway

import (
	"github.com/IodeSystems/graphql-go"

	"github.com/iodesystems/gwag/gw/ir"
)

// IRTypeNaming, IRTypeBuilderOptions, IRTypeBuilder are re-exported
// from ir for backward compatibility. New code should import ir directly.
type (
	IRTypeNaming         = ir.IRTypeNaming
	IRTypeBuilderOptions = ir.IRTypeBuilderOptions
	IRTypeBuilder        = ir.IRTypeBuilder
)

var (
	NewIRTypeBuilder = ir.NewIRTypeBuilder
)

// RuntimeOptions configures RenderGraphQLRuntime.
type RuntimeOptions struct {
	JSONType           *graphql.Scalar
	LongType           *graphql.Scalar
	SharedProtoBuilder *ir.IRTypeBuilder
	StableVN           map[string]int
}

// RenderGraphQLRuntime delegates to ir.RenderGraphQLRuntime.
func RenderGraphQLRuntime(svcs []*ir.Service, registry *ir.DispatchRegistry, opts RuntimeOptions) (*graphql.Schema, error) {
	return ir.RenderGraphQLRuntime(svcs, registry, ir.RuntimeOptions{
		JSONType:           opts.JSONType,
		LongType:           opts.LongType,
		SharedProtoBuilder: opts.SharedProtoBuilder,
		StableVN:           opts.StableVN,
	})
}

// RenderGraphQLRuntimeFields delegates to ir.RenderGraphQLRuntimeFields.
func RenderGraphQLRuntimeFields(svcs []*ir.Service, registry *ir.DispatchRegistry, opts RuntimeOptions) (graphql.Fields, graphql.Fields, graphql.Fields, error) {
	return ir.RenderGraphQLRuntimeFields(svcs, registry, ir.RuntimeOptions{
		JSONType:           opts.JSONType,
		LongType:           opts.LongType,
		SharedProtoBuilder: opts.SharedProtoBuilder,
		StableVN:           opts.StableVN,
	})
}
