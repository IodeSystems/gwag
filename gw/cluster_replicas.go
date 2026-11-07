package gateway

import (
	"fmt"
	"strconv"
	"strings"
)

// ClusterReplicas is the replica policy for the cluster's JetStream
// KV buckets. The gateway raises the bucket replica count as peers
// join; this policy maps the live peer count to that count.
//
//   - "odd" (default): the largest odd number <= min(live, 3). Two
//     live peers stay at R1: a 2-replica group has no quorum when
//     either member dies, so R2 is worse than R1 for availability.
//   - "auto": min(live, 3). Reaches R2 while a cluster forms.
//   - "N" (1..5): fixed N, capped at the live peer count.
//
// The empty value is "odd".
type ClusterReplicas string

const (
	ClusterReplicasOdd  ClusterReplicas = "odd"
	ClusterReplicasAuto ClusterReplicas = "auto"

	// maxFixedReplicas is the most replicas JetStream allows.
	maxFixedReplicas = 5
)

// ParseClusterReplicas validates a policy string ("odd", "auto" or
// an integer 1..5). The empty string is "odd".
func ParseClusterReplicas(s string) (ClusterReplicas, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "odd":
		return ClusterReplicasOdd, nil
	case "auto":
		return ClusterReplicasAuto, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxFixedReplicas {
		return "", fmt.Errorf("invalid cluster replicas %q: want odd, auto, or an integer 1..%d", s, maxFixedReplicas)
	}
	return ClusterReplicas(strconv.Itoa(n)), nil
}

// Validate reports whether the policy is well formed.
func (p ClusterReplicas) Validate() error {
	_, err := ParseClusterReplicas(string(p))
	return err
}

// For returns the replica count for live peers. The result is >= 1.
// An invalid policy behaves as "odd"; Validate catches it at boot.
func (p ClusterReplicas) For(live int) int {
	if live < 1 {
		live = 1
	}
	capped := live
	if capped > maxReplicas {
		capped = maxReplicas
	}
	parsed, err := ParseClusterReplicas(string(p))
	if err != nil {
		parsed = ClusterReplicasOdd
	}
	switch parsed {
	case ClusterReplicasAuto:
		return capped
	case ClusterReplicasOdd:
		if capped%2 == 0 {
			capped--
		}
		return capped
	}
	n, _ := strconv.Atoi(string(parsed))
	if n > live {
		n = live
	}
	return n
}

// WithClusterReplicas sets the replica policy for the cluster KV
// buckets: "odd" (default), "auto", or a fixed integer. See
// ClusterReplicas. An invalid value panics in New; command-line
// callers validate first with ParseClusterReplicas.
//
// Stability: stable
func WithClusterReplicas(p ClusterReplicas) Option {
	return func(cfg *config) { cfg.clusterReplicas = p }
}
