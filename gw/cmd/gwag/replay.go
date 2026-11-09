package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	gateway "github.com/iodesystems/gwag/v2/gw"
)

// replayVerifyOptions turns the gwag up --replay-verify-* flags into
// gateway options: target (blue) records a sample of GraphQL requests
// and posts them to green's verify endpoint; endpoint (green) serves
// that endpoint.
func replayVerifyOptions(target, tokenFile string, rate float64, endpoint bool) ([]gateway.Option, error) {
	var opts []gateway.Option
	if endpoint {
		opts = append(opts, gateway.WithReplayVerifyEndpoint())
	}
	if target == "" {
		if tokenFile != "" {
			return nil, errors.New("--replay-verify-token-file needs --replay-verify-target")
		}
		return opts, nil
	}
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return nil, fmt.Errorf("--replay-verify-target %q: want an http(s) URL", target)
	}
	if rate <= 0 || rate > 1 {
		return nil, fmt.Errorf("--replay-verify-sample-rate %v: want a fraction in (0, 1]", rate)
	}
	ro := gateway.ReplayVerifyOptions{Target: target, SampleRate: rate}
	if tokenFile != "" {
		// The file is read for every post, so blue can start before
		// green's system key exists, and a rotated key is picked up
		// without a restart.
		ro.Client = &http.Client{Transport: tokenFileTransport{path: tokenFile, base: http.DefaultTransport}}
	}
	return append(opts, gateway.WithReplayVerify(ro)), nil
}

// tokenFileTransport sets Authorization: Bearer <file contents> on
// every request. A missing or empty file fails the request.
type tokenFileTransport struct {
	path string
	base http.RoundTripper
}

func (t tokenFileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("replay verify token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return nil, fmt.Errorf("replay verify token: %s is empty", t.path)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(req)
}
