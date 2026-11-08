// Package deploy renders deployment files for a gwag cluster: Docker
// Compose, plain Kubernetes YAML (also for minikube) and an ECS
// (Fargate) CloudFormation template. Pure generation: no I/O, no
// platform CLI. The gwag deploy command writes the files and runs the
// platform tools.
//
// Every platform gets the same shape: one node per replica with a
// stable DNS name, one persistent volume per node, readiness on
// /api/health, a stop grace period longer than the drain
// (shutdown delay + 2 x timeout), the admin token as a platform secret,
// and at most one node down during an update.
package deploy

import (
	"fmt"
	"sort"
	"strings"
)

// Container ports. The same on every platform and inside every
// container.
const (
	HTTPPort    = 8080
	CPPort      = 50090
	NATSClient  = 14222
	NATSCluster = 14248

	// ShutdownDelay and ShutdownTimeout are passed to gwag up. Worst
	// case from SIGTERM to exit is delay + 2 x timeout = 70 s.
	ShutdownDelay   = "10s"
	ShutdownTimeout = "30s"
	// StopGraceSeconds is the platform stop timeout. Above 70 s.
	StopGraceSeconds = 90

	// DefaultImage is the local tag `gwag deploy up` builds.
	DefaultImage = "gwag:dev"
)

// Platforms accepted by Generate.
const (
	Compose  = "compose"
	K8s      = "k8s"
	Minikube = "minikube"
	ECS      = "ecs"
)

// Spec is the input of every generator.
type Spec struct {
	Platform string
	Replicas int
	Image    string
	// Token is the hex admin token written into the secret files.
	// ECS ignores it: Secrets Manager generates the token there.
	Token string

	// Compose only.
	Bind     string // host address ports are published on (default 127.0.0.1)
	HTTPBase int    // host port of node 1; node i gets +i-1 (default 8080)
	CPBase   int    // host control-plane port of node 1 (default 50090)
	NATSBase int    // host NATS client port of node 1 (default 14222)

	// Kubernetes only.
	Namespace string // default gwag
	Storage   string // volume size, default 1Gi
}

// File is one generated file. Path is relative to the output directory.
type File struct {
	Path string
	Mode uint32
	Data []byte
}

// Normalize fills defaults.
func (s Spec) Normalize() Spec {
	if s.Image == "" {
		s.Image = DefaultImage
	}
	if s.Replicas == 0 {
		s.Replicas = 1
	}
	if s.Bind == "" {
		s.Bind = "127.0.0.1"
	}
	if s.HTTPBase == 0 {
		s.HTTPBase = HTTPPort
	}
	if s.CPBase == 0 {
		s.CPBase = CPPort
	}
	if s.NATSBase == 0 {
		s.NATSBase = NATSClient
	}
	if s.Namespace == "" {
		s.Namespace = "gwag"
	}
	if s.Storage == "" {
		s.Storage = "1Gi"
	}
	return s
}

// ValidateReplicas rejects what cannot work and returns warnings for
// what works without fault tolerance. 1 and odd >= 3 are accepted
// silently.
func ValidateReplicas(n int) (warnings []string, err error) {
	switch {
	case n < 1:
		return nil, fmt.Errorf("--replicas %d: need at least 1", n)
	case n > 9:
		return nil, fmt.Errorf("--replicas %d: more than 9 nodes is not supported", n)
	case n == 2:
		return []string{"2 replicas have no fault tolerance: the odd replica policy keeps the buckets at R1, and NATS needs both nodes up to elect a meta leader. Use 1 or 3"}, nil
	case n%2 == 0:
		return []string{fmt.Sprintf("%d replicas tolerate no more failures than %d: use an odd number", n, n-1)}, nil
	}
	return nil, nil
}

// Generate renders the files for s.Platform.
func Generate(s Spec) ([]File, error) {
	s = s.Normalize()
	if _, err := ValidateReplicas(s.Replicas); err != nil {
		return nil, err
	}
	var files []File
	var err error
	switch s.Platform {
	case Compose:
		files, err = genCompose(s)
	case K8s, Minikube:
		files, err = genK8s(s)
	case ECS:
		files, err = genECS(s)
	default:
		return nil, fmt.Errorf("unknown platform %q (compose, k8s, minikube, ecs)", s.Platform)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// nodeArgs is the gwag command line of one node (after the binary).
// peers is empty for a standalone node.
func nodeArgs(nodeName string, peers []string) []string {
	args := []string{
		"up",
		"--data-dir=/data",
		"--no-context-write",
		fmt.Sprintf("--addr=:%d", HTTPPort),
		fmt.Sprintf("--control-plane=:%d", CPPort),
		fmt.Sprintf("--nats-listen=:%d", NATSClient),
		fmt.Sprintf("--nats-cluster=:%d", NATSCluster),
		"--node-name=" + nodeName,
		"--shutdown-delay=" + ShutdownDelay,
		"--shutdown-timeout=" + ShutdownTimeout,
	}
	for _, p := range peers {
		args = append(args, "--nats-peer="+p)
	}
	return args
}

// yamlList renders args as a block list at the given indent.
func yamlList(indent string, args []string) string {
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "%s- %s\n", indent, yamlQuote(a))
	}
	return strings.TrimRight(b.String(), "\n")
}

func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

const secretsIgnore = "# Admin token. Do not commit.\n*\n!.gitignore\n"

func secretsGitignore() File {
	return File{Path: "secrets/.gitignore", Mode: 0o644, Data: []byte(secretsIgnore)}
}
