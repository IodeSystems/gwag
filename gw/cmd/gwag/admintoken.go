package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

// adminTokenOption pins the admin bearer token from a hex file, or from
// the GWAG_ADMIN_TOKEN env var when file is empty. Returns nil (no
// option) when neither is set, so the gateway keeps its own
// generate-and-persist behaviour. Deployments from `gwag deploy` hand
// every node one platform-managed secret this way.
func adminTokenOption(file string) (gateway.Option, error) {
	var raw string
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	case os.Getenv("GWAG_ADMIN_TOKEN") != "":
		raw = os.Getenv("GWAG_ADMIN_TOKEN")
	default:
		return nil, nil
	}
	tok, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("token is not hex: %w", err)
	}
	if len(tok) < 16 {
		return nil, fmt.Errorf("token is %d bytes; need at least 16", len(tok))
	}
	return gateway.WithAdminToken(tok), nil
}
