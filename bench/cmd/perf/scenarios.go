package main

import (
	"fmt"
	"sort"
	"strings"
)

// scenarioPreset wires a scenario name to a default GraphQL query +
// a one-line description the help text surfaces. The operator can
// always override the default with explicit --query.
type scenarioPreset struct {
	name        string
	query       string
	description string
	// requiresNamespaces lists the namespaces the gateway must have
	// registered for this scenario's query to resolve. Surfaced in
	// help text so the operator knows what to bring up beforehand.
	requiresNamespaces []string
}

// scenarioPresets is the built-in registry. Order is the canonical
// order the cross-scenario orchestrator (`perf all`) walks.
var scenarioPresets = []scenarioPreset{
	{
		name:               "proto",
		description:        "pure proto/gRPC backend (greeter); baseline for native-format dispatch cost",
		query:              `{ greeter { hello(name: "world") { greeting } } }`,
		requiresNamespaces: []string{"greeter"},
	},
	{
		name:               "openapi",
		description:        "pure OpenAPI/HTTP backend (hello_openapi); same Hello shape via HTTP/JSON",
		query:              `{ hello_openapi { Hello(input: { name: "world" }) { greeting } } }`,
		requiresNamespaces: []string{"hello_openapi"},
	},
	{
		name:               "mixed",
		description:        "50/50 fan-out: one inbound GraphQL request dispatches to both proto + openapi backends",
		query:              `{ greeter { hello(name: "world") { greeting } } hello_openapi { Hello(input: { name: "world" }) { greeting } } }`,
		requiresNamespaces: []string{"greeter", "hello_openapi"},
	},
}

// scenarioPresetByName returns the preset whose name matches, or nil
// when the name is a custom label (in which case --query must be
// explicitly provided).
func scenarioPresetByName(name string) *scenarioPreset {
	for i := range scenarioPresets {
		if scenarioPresets[i].name == name {
			return &scenarioPresets[i]
		}
	}
	return nil
}

// scenarioPresetNames returns the registry names in stable order for
// help text + the --scenarios default.
func scenarioPresetNames() []string {
	out := make([]string, 0, len(scenarioPresets))
	for _, p := range scenarioPresets {
		out = append(out, p.name)
	}
	return out
}

// scenarioHelp formats the preset registry for stderr — used by
// `perf run --help` and `perf all --help`.
func scenarioHelp() string {
	var b strings.Builder
	b.WriteString("Built-in scenario presets (override --query to use a custom shape):\n")
	names := make([]string, len(scenarioPresets))
	for i, p := range scenarioPresets {
		names[i] = p.name
	}
	sort.Strings(names)
	for _, n := range names {
		p := scenarioPresetByName(n)
		fmt.Fprintf(&b, "  %-8s  %s\n", p.name, p.description)
		fmt.Fprintf(&b, "            requires namespaces: %s\n", strings.Join(p.requiresNamespaces, ", "))
	}
	return b.String()
}
