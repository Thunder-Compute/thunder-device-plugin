package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// journalRunner is a node whose journal emits a fixed set of lines and then
// blocks, the way `journalctl --follow` does.
type journalRunner struct {
	lines []string

	mu       sync.Mutex
	commands [][]string
}

func (r *journalRunner) CombinedOutput(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

func (r *journalRunner) RunShell(context.Context, string, string) error { return nil }

func (r *journalRunner) Stream(ctx context.Context, onLine func(string), name string, args ...string) error {
	r.mu.Lock()
	r.commands = append(r.commands, append([]string{name}, args...))
	r.mu.Unlock()

	for _, line := range r.lines {
		onLine(line)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *journalRunner) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.commands...)
}

// captureLogs redirects the daemon's log output for the duration of a test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buffer := &syncBuffer{}
	flags := log.Flags()
	log.SetOutput(buffer)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return buffer
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// thunderd's journal is what someone debugging a node would read over SSH. The
// daemon republishes it so `kubectl logs` shows it instead.
func TestFollowThunderdLogsRepublishesTheJournal(t *testing.T) {
	logs := captureLogs(t)
	runner := &journalRunner{lines: []string{"listening on 0.0.0.0:2280", "gpu 0 attached"}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		followThunderdLogs(ctx, runner, DefaultThunderdLogUnit)
	}()

	waitFor(t, func() bool { return strings.Contains(logs.String(), "gpu 0 attached") })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("followThunderdLogs did not return after cancel")
	}

	if !strings.Contains(logs.String(), "thunderd: listening on 0.0.0.0:2280") {
		t.Fatalf("journal lines were not attributed to thunderd:\n%s", logs.String())
	}

	commands := runner.recorded()
	if len(commands) == 0 {
		t.Fatal("the journal was never read")
	}
	command := strings.Join(commands[0], " ")
	// --lines 0 starts at the end of the journal: a reconnect must not replay
	// what the pod already logged.
	for _, want := range []string{"journalctl", "--unit " + DefaultThunderdLogUnit, "--follow", "--lines 0"} {
		if !strings.Contains(command, want) {
			t.Fatalf("journal command missing %q: %s", want, command)
		}
	}
}

// An operator who would rather read thunderd's logs on the node can turn the
// stream off, and nothing runs on the node when they do.
func TestFollowThunderdLogsCanBeTurnedOff(t *testing.T) {
	logs := captureLogs(t)
	runner := &journalRunner{lines: []string{"listening"}}

	followThunderdLogs(context.Background(), runner, "off")

	if got := runner.recorded(); len(got) != 0 {
		t.Fatalf("the journal was read anyway: %#v", got)
	}
	if !strings.Contains(logs.String(), "not following thunderd logs") {
		t.Fatalf("the daemon did not say it was not following logs:\n%s", logs.String())
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
