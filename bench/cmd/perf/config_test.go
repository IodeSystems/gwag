package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPerfConfig_BundledDefaults(t *testing.T) {
	// Walk up from the test's working directory to find the bundled
	// scenarios YAML. The bundled file pins the contract — proto must
	// be present and minimally complete so a fresh checkout runs.
	cfg, err := loadPerfConfig(repoRelative(t, "bench/perf-scenarios.yaml"))
	if err != nil {
		t.Fatalf("load bundled config: %v", err)
	}
	var sawProto bool
	for _, s := range cfg.Scenarios {
		if s.Name == "proto" {
			sawProto = true
			if s.Query == "" {
				t.Error("proto scenario has empty query")
			}
			if len(s.Sweep.Steps) == 0 {
				t.Error("proto scenario has no sweep steps")
			}
			if len(s.Services) == 0 {
				t.Error("proto scenario declares no services")
			}
		}
	}
	if !sawProto {
		t.Errorf("bundled config missing proto scenario; got: %+v", cfg.Scenarios)
	}
}

func TestLoadPerfConfig_DefaultsAndValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name:    "missing name",
			yaml:    "scenarios:\n  - query: '{ a }'\n    sweep:\n      steps: [100]\n",
			wantErr: true,
		},
		{
			name:    "missing query",
			yaml:    "scenarios:\n  - name: x\n    sweep:\n      steps: [100]\n",
			wantErr: true,
		},
		{
			name:    "missing steps",
			yaml:    "scenarios:\n  - name: x\n    query: '{ a }'\n    sweep:\n      reps: 3\n",
			wantErr: true,
		},
		{
			name:    "invalid duration",
			yaml:    "scenarios:\n  - name: x\n    query: '{ a }'\n    sweep:\n      steps: [100]\n      duration: '5banana'\n",
			wantErr: true,
		},
		{
			name:    "invalid upstream_latency",
			yaml:    "scenarios:\n  - name: x\n    query: '{ a }'\n    sweep:\n      steps: [100]\n    upstream_latency: nope\n",
			wantErr: true,
		},
		{
			name:    "minimal — defaults apply",
			yaml:    "scenarios:\n  - name: x\n    query: '{ a }'\n    sweep:\n      steps: [100, 200]\n",
			wantErr: false,
		},
		{
			name:    "no scenarios",
			yaml:    "scenarios: []\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp, err := os.CreateTemp(t.TempDir(), "perf-*.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tmp.WriteString(tc.yaml); err != nil {
				t.Fatal(err)
			}
			tmp.Close()
			cfg, err := loadPerfConfig(tmp.Name())
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got cfg=%+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			s := cfg.Scenarios[0]
			if s.Sweep.Reps != 3 {
				t.Errorf("default reps not applied: %d", s.Sweep.Reps)
			}
			if s.Sweep.Duration != "5s" {
				t.Errorf("default duration not applied: %q", s.Sweep.Duration)
			}
			if s.Target == "" {
				t.Error("default target not applied")
			}
		})
	}
}

func repoRelative(t *testing.T, rel string) string {
	t.Helper()
	// The test binary's CWD is the package dir. Walk up to find
	// the bundled YAML (rooted at repo root).
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate %s starting from %s", rel, dir)
	return ""
}
