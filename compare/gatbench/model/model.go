// Package model is the fixed workload every gatbench implementation
// serves. Keeping the type, the data, and the lookup identical across
// gat, gqlgen, connect-go and grpc-gateway is what makes the
// benchmark numbers comparable — any difference is the framework, not
// the business logic.
package model

import "fmt"

// Project is the single resource under benchmark. Two scalar fields
// and one string list: small enough that serialization doesn't
// dominate, wide enough that per-field executor cost is visible.
type Project struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags,omitempty"`
}

// FanOut is the row count the list benchmarks request. Chosen to sit
// in the range a real list endpoint returns — big enough that
// per-field cost outweighs per-request fixed cost, small enough to
// stay a realistic page.
const FanOut = 25

// Store is the in-memory dataset. Read-only after init, so no lock is
// needed and no lock contention biases a parallel benchmark.
var Store = buildStore()

func buildStore() []Project {
	out := make([]Project, FanOut)
	for i := range out {
		out[i] = Project{
			ID:   fmt.Sprintf("p%d", i),
			Name: fmt.Sprintf("Project %d", i),
			Tags: []string{"core"},
		}
	}
	return out
}

// StorePtrs is Store as pointers. gqlgen's generated models use
// []*Project for object lists; gat serves []Project. Prebuilding both
// views means neither side pays a per-request conversion the other
// doesn't — the benchmark compares frameworks, not my schema mapping.
var StorePtrs = buildStorePtrs()

func buildStorePtrs() []*Project {
	out := make([]*Project, len(Store))
	for i := range Store {
		out[i] = &Store[i]
	}
	return out
}

// Get returns the project with the given id, or false. Linear scan
// over 25 entries is deliberate: a map lookup and a scan are both
// noise next to the framework cost, and a scan keeps every
// implementation's handler byte-identical in cost.
func Get(id string) (Project, bool) {
	for _, p := range Store {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

// List returns the first limit projects (all of them when limit <= 0
// or over the dataset size).
func List(limit int) []Project {
	return Store[:clampLimit(limit)]
}

// ListPtr is List over the pointer view.
func ListPtr(limit int) []*Project {
	return StorePtrs[:clampLimit(limit)]
}

// GetPtr is Get over the pointer view; nil when absent.
func GetPtr(id string) *Project {
	for _, p := range StorePtrs {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > len(Store) {
		return len(Store)
	}
	return limit
}
