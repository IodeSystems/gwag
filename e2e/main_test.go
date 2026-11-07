//go:build e2e

// Package e2e holds the end-to-end stories: real gateway and fixture
// processes driven through e2e/harness. Run with
//
//	go test -tags e2e ./e2e/...
package e2e

import (
	"os"
	"testing"

	"github.com/iodesystems/gwag/v2/e2e/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }
