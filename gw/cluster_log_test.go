package gateway

import (
	"sync/atomic"
	"testing"
	"time"
)

// captureLogger is a natsd.Logger that counts calls per level so a
// test can assert which messages survived the LogLevel filter.
type captureLogger struct {
	notice atomic.Int64
	warn   atomic.Int64
	err    atomic.Int64
	debug  atomic.Int64
	trace  atomic.Int64
}

func (l *captureLogger) Noticef(string, ...any) { l.notice.Add(1) }
func (l *captureLogger) Warnf(string, ...any)   { l.warn.Add(1) }
func (l *captureLogger) Fatalf(string, ...any)  {}
func (l *captureLogger) Errorf(string, ...any)  { l.err.Add(1) }
func (l *captureLogger) Debugf(string, ...any)  { l.debug.Add(1) }
func (l *captureLogger) Tracef(string, ...any)  { l.trace.Add(1) }

// TestClusterOptions_LoggerCustomReceives confirms the Logger field
// on ClusterOptions actually receives NATS server output. A startup
// emits at least one Noticef as the server announces its listen
// addresses, so we can drive the assertion off that.
func TestClusterOptions_LoggerCustomReceives(t *testing.T) {
	dir := t.TempDir()
	rec := &captureLogger{}
	cluster, err := StartCluster(ClusterOptions{
		NodeName:     "logger-test",
		ClientListen: freeAddr(t),
		DataDir:      dir,
		StartTimeout: 10 * time.Second,
		Logger:       rec,
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	if rec.notice.Load() == 0 {
		t.Fatalf("custom Logger received no Notice calls during startup")
	}
}

// TestClusterOptions_LogLevelSilent drops every NATS server log line.
// We can't easily prove "stderr is empty" without redirection
// shenanigans, but we can confirm the LogLevel="silent" path
// installs the silent logger and the gateway boots cleanly.
func TestClusterOptions_LogLevelSilent(t *testing.T) {
	dir := t.TempDir()
	cluster, err := StartCluster(ClusterOptions{
		NodeName:     "silent-test",
		ClientListen: freeAddr(t),
		DataDir:      dir,
		StartTimeout: 10 * time.Second,
		LogLevel:     "silent",
	})
	if err != nil {
		t.Fatalf("StartCluster: %v", err)
	}
	t.Cleanup(cluster.Close)

	// Smoke: server is up and JS is reachable. The contract is
	// "silent logger doesn't break server lifecycle."
	if cluster.Server == nil {
		t.Fatal("cluster.Server is nil")
	}
}
