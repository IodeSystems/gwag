package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HostSpecs captures everything an adopter needs to calibrate a
// reported number against their own hardware. Fields are
// best-effort: anything we can't determine is left empty rather
// than guessed. Surfacing partial data is fine; lying isn't.
type HostSpecs struct {
	// When the snapshot was taken (UTC, RFC3339).
	CapturedAt string

	// CPU model string and core count. Cores is logical CPUs (what
	// GOMAXPROCS sees) — that's the number that matters for tuning.
	CPUModel string
	CPUCores int

	// Total physical memory in bytes. Zero when undetectable.
	MemBytes uint64

	// OS / kernel release. Strings vary across distros; we surface
	// them verbatim instead of normalising.
	OS         string
	OSVersion  string
	Kernel     string
	Arch       string
	GoVersion  string
	GatewayRev string // git HEAD short SHA, "" if not in a repo
	Dirty      bool   // git working tree dirty at capture time
}

// CollectSpecs gathers host info best-effort. Errors are absorbed
// into empty fields — the caller renders whatever made it through.
func CollectSpecs() HostSpecs {
	s := HostSpecs{
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		CPUCores:   runtime.NumCPU(),
		Arch:       runtime.GOARCH,
		OS:         runtime.GOOS,
		GoVersion:  runtime.Version(),
	}
	if runtime.GOOS == "linux" {
		if model, ok := readCPUModelLinux(); ok {
			s.CPUModel = model
		}
		if mem, ok := readMemTotalLinux(); ok {
			s.MemBytes = mem
		}
		if osName, osVer, ok := readOSReleaseLinux(); ok {
			s.OS = osName
			s.OSVersion = osVer
		}
		if k, ok := unameRelease(); ok {
			s.Kernel = k
		}
	}
	if rev, dirty, ok := gitRev(); ok {
		s.GatewayRev = rev
		s.Dirty = dirty
	}
	return s
}

// readCPUModelLinux parses /proc/cpuinfo for the model string. All
// cores on a single-socket box report the same model, so the first
// hit is good enough. Multi-socket asymmetric boxes are rare enough
// we don't try to be clever.
func readCPUModelLinux() (string, bool) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	defer f.Close()
	return scanCPUModel(f)
}

func scanCPUModel(r io.Reader) (string, bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// x86: "model name". arm64: "Model" or empty (then "Hardware").
		for _, key := range []string{"model name", "Model", "Hardware", "CPU implementer"} {
			if v, ok := matchProcKV(line, key); ok && v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// matchProcKV parses one /proc/cpuinfo or /proc/meminfo line of the
// form "key<spaces>:<spaces>value". Returns ("", false) when the
// line's key doesn't match.
func matchProcKV(line, key string) (string, bool) {
	k, v, ok := strings.Cut(line, ":")
	if !ok {
		return "", false
	}
	if strings.TrimSpace(k) != key {
		return "", false
	}
	return strings.TrimSpace(v), true
}

// readMemTotalLinux pulls MemTotal from /proc/meminfo. The value is
// in kB per the kernel's contract.
func readMemTotalLinux() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return scanMemTotal(f)
}

func scanMemTotal(r io.Reader) (uint64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		v, ok := matchProcKV(sc.Text(), "MemTotal")
		if !ok {
			continue
		}
		// e.g. "16321820 kB"
		v = strings.TrimSuffix(v, " kB")
		v = strings.TrimSpace(v)
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return n * 1024, true
	}
	return 0, false
}

// readOSReleaseLinux reads /etc/os-release for the distro name +
// version. Skipped quietly on non-Linux.
func readOSReleaseLinux() (string, string, bool) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	return scanOSRelease(f)
}

func scanOSRelease(r io.Reader) (string, string, bool) {
	var name, version string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		k, rawV, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v := strings.Trim(rawV, `"`)
		switch k {
		case "NAME":
			name = v
		case "VERSION", "VERSION_ID":
			if version == "" {
				version = v
			}
		}
	}
	if name == "" {
		return "", "", false
	}
	return name, version, true
}

// unameRelease shells to `uname -r`. Cheaper than reading
// /proc/sys/kernel/osrelease but functionally identical on linux;
// we use it so future ports to darwin/bsd work without rework.
func unameRelease() (string, bool) {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// gitRev returns HEAD's short SHA and whether the working tree is
// dirty. Both are best-effort: ("", false, false) when we're not in
// a git repo or the binary isn't on PATH.
func gitRev() (string, bool, bool) {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "", false, false
	}
	rev := strings.TrimSpace(string(out))
	dirty := false
	if st, err := exec.Command("git", "status", "--porcelain").Output(); err == nil {
		dirty = len(strings.TrimSpace(string(st))) > 0
	}
	return rev, dirty, true
}

// Markdown renders the specs as the report header block. Stable
// shape — the sweep driver will template this into docs/perf.md.
// Empty fields collapse to "n/a" so the table doesn't have visible
// holes that confuse readers.
func (s HostSpecs) Markdown() string {
	na := func(v string) string {
		if v == "" {
			return "n/a"
		}
		return v
	}
	rev := na(s.GatewayRev)
	if s.Dirty {
		rev += " (dirty)"
	}
	osLine := na(s.OS)
	if s.OSVersion != "" {
		osLine = fmt.Sprintf("%s %s", osLine, s.OSVersion)
	}
	mem := "n/a"
	if s.MemBytes > 0 {
		mem = fmt.Sprintf("%.1f GiB", float64(s.MemBytes)/(1024*1024*1024))
	}
	var b strings.Builder
	b.WriteString("| Field | Value |\n")
	b.WriteString("|---|---|\n")
	fmt.Fprintf(&b, "| Captured at | %s |\n", s.CapturedAt)
	fmt.Fprintf(&b, "| CPU | %s |\n", na(s.CPUModel))
	fmt.Fprintf(&b, "| Cores (logical) | %d |\n", s.CPUCores)
	fmt.Fprintf(&b, "| RAM | %s |\n", mem)
	fmt.Fprintf(&b, "| OS | %s |\n", osLine)
	fmt.Fprintf(&b, "| Kernel | %s |\n", na(s.Kernel))
	fmt.Fprintf(&b, "| Arch | %s |\n", na(s.Arch))
	fmt.Fprintf(&b, "| Go | %s |\n", na(s.GoVersion))
	fmt.Fprintf(&b, "| Gateway rev | %s |\n", rev)
	return b.String()
}

func runSpecs(args []string) error {
	fs := flag.NewFlagSet("specs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: perf specs")
		fmt.Fprintln(fs.Output(), "Print host specs as the markdown header docs/perf.md will use.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected positional args: %v", fs.Args())
	}
	fmt.Print(CollectSpecs().Markdown())
	return nil
}
