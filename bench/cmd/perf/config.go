package main

import (
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// perfConfig is the YAML wire shape for bench/perf-scenarios.yaml.
// Defaults live at bench/perf-scenarios.yaml in the repo; an operator
// can pass --config to point at a custom file (e.g. one tuned for a
// performance regression).
type perfConfig struct {
	Scenarios []perfScenario `yaml:"scenarios"`
}

// perfScenario is one row in the YAML — a name, the upstream
// services it needs registered, the query, and the sweep config.
type perfScenario struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Services    []perfService  `yaml:"services,omitempty"`
	Target      string         `yaml:"target"`
	Query       string         `yaml:"query"`
	Sweep       perfSweepCfg   `yaml:"sweep"`
	// UpstreamLatency is the artificial delay each service is
	// configured for (matches a `delay` setting on the services
	// list). Metadata only — the orchestrator passes this through
	// to runSweep as the --upstream-latency stamp.
	UpstreamLatency string `yaml:"upstream_latency,omitempty"`
}

// perfService is one upstream service the orchestrator's setup
// phase will register. Kind is the managed-kind name (`greeter`
// today; the bench script's `service add` semantics decide what's
// supported). Delay is an optional synthetic per-call sleep for
// "gateway on top of realistic upstream" runs.
type perfService struct {
	Kind  string `yaml:"kind"`
	Delay string `yaml:"delay,omitempty"`
}

// perfSweepCfg mirrors runSweep's flags: rungs of target RPS, reps
// per rung, per-rep duration. Validated against the same rules
// runSweep enforces.
type perfSweepCfg struct {
	Steps    []int  `yaml:"steps"`
	Reps     int    `yaml:"reps"`
	Duration string `yaml:"duration"`
	NoWarmup bool   `yaml:"no_warmup,omitempty"`
	NoKnee   bool   `yaml:"no_knee,omitempty"`
}

// loadPerfConfig decodes the YAML at path, fills in any obvious
// defaults (so a slim scenario doesn't have to enumerate everything),
// and validates that each scenario can be dispatched.
func loadPerfConfig(path string) (*perfConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg perfConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Scenarios) == 0 {
		return nil, fmt.Errorf("%s: no scenarios defined", path)
	}
	for i := range cfg.Scenarios {
		s := &cfg.Scenarios[i]
		if s.Name == "" {
			return nil, fmt.Errorf("scenario #%d: name is required", i+1)
		}
		if s.Query == "" {
			return nil, fmt.Errorf("scenario %q: query is required", s.Name)
		}
		if s.Target == "" {
			s.Target = "http://localhost:18080/api/graphql"
		}
		if len(s.Sweep.Steps) == 0 {
			return nil, fmt.Errorf("scenario %q: sweep.steps is required", s.Name)
		}
		if s.Sweep.Reps <= 0 {
			s.Sweep.Reps = 3
		}
		if s.Sweep.Duration == "" {
			s.Sweep.Duration = "5s"
		}
		if _, err := time.ParseDuration(s.Sweep.Duration); err != nil {
			return nil, fmt.Errorf("scenario %q: invalid sweep.duration %q: %w", s.Name, s.Sweep.Duration, err)
		}
		if s.UpstreamLatency != "" {
			if _, err := time.ParseDuration(s.UpstreamLatency); err != nil {
				return nil, fmt.Errorf("scenario %q: invalid upstream_latency %q: %w", s.Name, s.UpstreamLatency, err)
			}
		}
		for j, svc := range s.Services {
			if svc.Kind == "" {
				return nil, fmt.Errorf("scenario %q service #%d: kind is required", s.Name, j+1)
			}
			if svc.Delay != "" {
				if _, err := time.ParseDuration(svc.Delay); err != nil {
					return nil, fmt.Errorf("scenario %q service %q: invalid delay %q: %w", s.Name, svc.Kind, svc.Delay, err)
				}
			}
		}
	}
	return &cfg, nil
}

