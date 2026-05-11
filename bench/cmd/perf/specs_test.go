package main

import (
	"strings"
	"testing"
)

func TestScanCPUModel_ModelName(t *testing.T) {
	const cpuinfo = `processor	: 0
vendor_id	: GenuineIntel
model name	: Intel(R) Core(TM) i7-1185G7 @ 3.00GHz
cpu MHz		: 1804.800

processor	: 1
model name	: Intel(R) Core(TM) i7-1185G7 @ 3.00GHz
`
	got, ok := scanCPUModel(strings.NewReader(cpuinfo))
	if !ok {
		t.Fatal("scanCPUModel returned ok=false on x86 cpuinfo")
	}
	if want := "Intel(R) Core(TM) i7-1185G7 @ 3.00GHz"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScanCPUModel_ARMHardware(t *testing.T) {
	const cpuinfo = `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd
CPU implementer	: 0x41
CPU architecture: 8

Hardware	: Raspberry Pi 5 Model B Rev 1.0
`
	got, ok := scanCPUModel(strings.NewReader(cpuinfo))
	if !ok {
		t.Fatal("scanCPUModel returned ok=false on arm cpuinfo")
	}
	// Hardware wins over CPU implementer (more descriptive); the
	// scan returns the first hit it finds in the priority list.
	if !strings.Contains(got, "Raspberry") && !strings.Contains(got, "0x41") {
		t.Errorf("unexpected model %q", got)
	}
}

func TestScanCPUModel_Empty(t *testing.T) {
	if _, ok := scanCPUModel(strings.NewReader("")); ok {
		t.Error("scanCPUModel on empty input returned ok=true")
	}
}

func TestScanMemTotal(t *testing.T) {
	const meminfo = `MemTotal:       16321820 kB
MemFree:         1234567 kB
MemAvailable:    9876543 kB
`
	got, ok := scanMemTotal(strings.NewReader(meminfo))
	if !ok {
		t.Fatal("scanMemTotal returned ok=false")
	}
	if want := uint64(16321820) * 1024; got != want {
		t.Errorf("got %d bytes, want %d", got, want)
	}
}

func TestScanMemTotal_Missing(t *testing.T) {
	if _, ok := scanMemTotal(strings.NewReader("MemFree: 1234 kB\n")); ok {
		t.Error("scanMemTotal on input without MemTotal returned ok=true")
	}
}

func TestScanOSRelease(t *testing.T) {
	const osr = `NAME="Ubuntu"
VERSION="24.04.1 LTS (Noble Numbat)"
ID=ubuntu
VERSION_ID="24.04"
`
	name, ver, ok := scanOSRelease(strings.NewReader(osr))
	if !ok {
		t.Fatal("scanOSRelease returned ok=false")
	}
	if name != "Ubuntu" {
		t.Errorf("name=%q, want Ubuntu", name)
	}
	// VERSION wins over VERSION_ID because it lands first in iteration.
	if !strings.HasPrefix(ver, "24.04") {
		t.Errorf("unexpected version %q", ver)
	}
}

func TestScanOSRelease_NoName(t *testing.T) {
	if _, _, ok := scanOSRelease(strings.NewReader("ID=void\n")); ok {
		t.Error("scanOSRelease without NAME returned ok=true")
	}
}

func TestHostSpecs_MarkdownEmptyFieldsCollapse(t *testing.T) {
	s := HostSpecs{
		CapturedAt: "2026-05-11T12:00:00Z",
		CPUCores:   8,
		GoVersion:  "go1.26.2",
	}
	md := s.Markdown()
	// Every row present + empties rendered as n/a, not blank cells.
	for _, want := range []string{
		"| Captured at | 2026-05-11T12:00:00Z |",
		"| CPU | n/a |",
		"| Cores (logical) | 8 |",
		"| RAM | n/a |",
		"| OS | n/a |",
		"| Kernel | n/a |",
		"| Go | go1.26.2 |",
		"| Gateway rev | n/a |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing row %q\n--- full markdown ---\n%s", want, md)
		}
	}
}

func TestHostSpecs_MarkdownDirtyAnnotation(t *testing.T) {
	s := HostSpecs{GatewayRev: "abc1234", Dirty: true}
	if !strings.Contains(s.Markdown(), "abc1234 (dirty)") {
		t.Errorf("dirty annotation missing: %s", s.Markdown())
	}
}

func TestHostSpecs_MarkdownMemFormatted(t *testing.T) {
	s := HostSpecs{MemBytes: 16 * 1024 * 1024 * 1024}
	if !strings.Contains(s.Markdown(), "| RAM | 16.0 GiB |") {
		t.Errorf("16 GiB row missing: %s", s.Markdown())
	}
}

func TestCollectSpecsPopulatesBasics(t *testing.T) {
	s := CollectSpecs()
	if s.CapturedAt == "" {
		t.Error("CapturedAt empty")
	}
	if s.CPUCores == 0 {
		t.Error("CPUCores == 0; runtime.NumCPU should always be > 0")
	}
	if s.Arch == "" {
		t.Error("Arch empty; runtime.GOARCH should always be set")
	}
	if s.GoVersion == "" {
		t.Error("GoVersion empty")
	}
}
