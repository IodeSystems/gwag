//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The images examples/events/kafka-stack.sh and the go-kafka /
// py-kafka test scripts pin. KAFKA_IMAGE and SR_IMAGE override them,
// as they do there.
const (
	defaultKafkaImage = "apache/kafka:4.3.1"
	defaultSRImage    = "confluentinc/cp-schema-registry:8.1.0"
)

var kafkaSeq atomic.Int32

// KafkaStack is a single-node Kafka and a Confluent Schema Registry in
// docker, on free host ports, removed when the test ends. The same
// container setup as examples/events/kafka-stack.sh.
type KafkaStack struct {
	// Brokers is the host address of the broker ("127.0.0.1:port").
	Brokers string
	// SRURL is the host URL of the registry.
	SRURL string
	// Name is the Kafka container; its listener for other containers on
	// the stack's network is Name:9094.
	Name string
	// SRName is the registry container.
	SRName string
}

// DockerAvailable says why docker cannot be used, or "".
func DockerAvailable() string {
	if _, err := exec.LookPath("docker"); err != nil {
		return "docker not on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if b, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return fmt.Sprintf("docker info: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return ""
}

// StartKafkaStack starts the stack and waits until the broker has
// started and the registry answers.
func StartKafkaStack(t testing.TB) *KafkaStack {
	t.Helper()
	kafkaImage, srImage := defaultKafkaImage, defaultSRImage
	if v := os.Getenv("KAFKA_IMAGE"); v != "" {
		kafkaImage = v
	}
	if v := os.Getenv("SR_IMAGE"); v != "" {
		srImage = v
	}
	name := fmt.Sprintf("gwag-e2e-kafka-%d-%d", os.Getpid(), kafkaSeq.Add(1))
	net := name + "-net"
	s := &KafkaStack{Name: name, SRName: name + "-sr"}
	kport, sport := FreePort(t), FreePort(t)
	docker := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, b)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", s.Name, s.SRName).Run()
		_ = exec.Command("docker", "network", "rm", net).Run()
	})
	docker("network", "create", net)
	// PLAINTEXT is for the host; INTERNAL for the registry container
	// (and the Confluent console tools run inside it).
	docker("run", "-d", "--name", name, "--network", net, "-p", fmt.Sprintf("127.0.0.1:%d:9092", kport),
		"-e", "KAFKA_NODE_ID=1",
		"-e", "KAFKA_PROCESS_ROLES=broker,controller",
		"-e", "KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093,INTERNAL://:9094",
		"-e", fmt.Sprintf("KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://127.0.0.1:%d,INTERNAL://%s:9094", kport, name),
		"-e", "KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER",
		"-e", "KAFKA_INTER_BROKER_LISTENER_NAME=INTERNAL",
		"-e", "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,INTERNAL:PLAINTEXT",
		"-e", "KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093",
		"-e", "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1",
		"-e", "KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1",
		"-e", "KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1",
		"-e", "KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0",
		kafkaImage)
	docker("run", "-d", "--name", s.SRName, "--network", net, "-p", fmt.Sprintf("127.0.0.1:%d:8081", sport),
		"-e", "SCHEMA_REGISTRY_HOST_NAME=sr",
		"-e", "SCHEMA_REGISTRY_LISTENERS=http://0.0.0.0:8081",
		"-e", fmt.Sprintf("SCHEMA_REGISTRY_KAFKASTORE_BOOTSTRAP_SERVERS=PLAINTEXT://%s:9094", name),
		srImage)
	s.Brokers = fmt.Sprintf("127.0.0.1:%d", kport)
	s.SRURL = fmt.Sprintf("http://127.0.0.1:%d", sport)

	// The advertised listener is the host port, which does not exist
	// inside the container: wait on the log line, as kafka-stack.sh does.
	Eventually(t, 2*time.Minute, "kafka logs 'Kafka Server started'", func() error {
		b, err := exec.Command("docker", "logs", name).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker logs: %v", err)
		}
		if !strings.Contains(string(b), "Kafka Server started") {
			return fmt.Errorf("not started")
		}
		return nil
	})
	Eventually(t, 3*time.Minute, "schema registry answers GET /subjects", func() error {
		resp, err := http.Get(s.SRURL + "/subjects")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})
	return s
}

// Exec runs a command in the registry container (it has the Confluent
// console tools) and returns its stdout.
func (s *KafkaStack) Exec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", s.SRName}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return string(b), fmt.Errorf("docker exec %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(b), nil
}

// Env is the environment the examples read: KAFKA_BROKERS and SR_URL.
func (s *KafkaStack) Env() []string {
	return []string{"KAFKA_BROKERS=" + s.Brokers, "SR_URL=" + s.SRURL}
}
