package eventgen

import (
	"regexp"
	"runtime/debug"
)

const gwagModule = "github.com/iodesystems/gwag/v2"

// pseudo matches a Go pseudo-version suffix ("-0.20261107004810-a2ef7c575d7c")
// or a dirty-tree marker.
var pseudo = regexp.MustCompile(`(\d{14}-[0-9a-f]{12}|\+dirty)`)

// GwagVersion returns the gwag module version of this binary: the tag
// for a tagged build, "(devel)" for anything else (a pseudo-version,
// a dirty tree, go run). Untagged builds all stamp the same string,
// so regenerating on another commit does not churn generated headers.
//
// Stability: experimental
func GwagVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	v := ""
	if bi.Main.Path == gwagModule {
		v = bi.Main.Version
	} else {
		for _, d := range bi.Deps {
			if d.Path == gwagModule {
				v = d.Version
				if d.Replace != nil {
					v = d.Replace.Version
				}
				break
			}
		}
	}
	return cleanVersion(v)
}

func cleanVersion(v string) string {
	if v == "" || v == "(devel)" || pseudo.MatchString(v) {
		return "(devel)"
	}
	return v
}

// depVersion returns the version of module path in this binary's
// build, or "latest".
func depVersion(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "latest"
	}
	for _, d := range bi.Deps {
		if d.Path == path && d.Version != "" {
			return d.Version
		}
	}
	return "latest"
}
