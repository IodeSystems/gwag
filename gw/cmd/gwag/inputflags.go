package main

import (
	"flag"

	gateway "github.com/iodesystems/gwag/v2/gw"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

// inputFlags are the request-input compatibility flags shared by
// `gwag up`, `gwag serve` and the flag-mode `gwag`.
type inputFlags struct {
	lossy      *bool
	legacyNull *bool
}

func addInputFlags(fs *flag.FlagSet) *inputFlags {
	return &inputFlags{
		legacyNull: fs.Bool("legacy-null-args", false, "Drop null arguments and null input-object fields before dispatch, as graphql-go v2 did. Default: an OpenAPI JSON body and MCP tool arguments get the null (proto fields and OpenAPI path/query params treat null as unset either way)"),
		lossy:      fs.Bool("allow-lossy-coercion-of-inputs", false, "Accept lossy client variables for Int, Float and ID as graphql-go v2 did (2.5 -> 2, true -> 1 for Int). Default: rejected; lossless values (\"2\", 2.0) are accepted either way"),
	}
}

func (f *inputFlags) options() []gateway.Option {
	var opts []gateway.Option
	if *f.lossy {
		opts = append(opts, gateway.WithLossyInputCoercion())
	}
	if *f.legacyNull {
		opts = append(opts, gateway.WithLegacyNullArgs())
	}
	return opts
}

// applyGat sets the flags on the gat lite path (gwag serve --openapi /
// --proto).
func (f *inputFlags) applyGat(g *gat.Gateway) {
	g.AllowLossyInputCoercion(*f.lossy).LegacyNullArgs(*f.legacyNull)
}
