package daemon

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
)

// maxLogLineBytes bounds one line of node output. Anything longer is a binary
// blob or a runaway message rather than a log line, and buffering it whole
// would put the node's memory at the mercy of whatever it printed.
const maxLogLineBytes = 1 << 20

type commandRunner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunShell runs a shell command on the node and republishes its output as
	// the daemon's own log, one line at a time under label. Node output that
	// went straight to the container's stdout arrived untimestamped and
	// unattributed, which is what made the installer's progress unreadable
	// next to the daemon's own lines.
	RunShell(ctx context.Context, label string, command string) error
	// Stream runs a command on the node and calls onLine for each line it
	// writes, returning when the command exits or ctx is cancelled. It is how
	// the daemon follows output that never ends, such as thunderd's journal.
	Stream(ctx context.Context, onLine func(string), name string, args ...string) error
}

type osCommandRunner struct {
	targetPID string
}

// command builds a command in the node's namespaces. Without a target PID it
// runs in the container, which is what the tests and a non-privileged
// deployment get.
func (r osCommandRunner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if r.targetPID == "" || r.targetPID == "0" {
		cmd = exec.CommandContext(ctx, name, args...)
	} else {
		nsenterArgs := append([]string{"-t", r.targetPID, "-m", "-u", "-i", "-n", "-p", "--", name}, args...)
		cmd = exec.CommandContext(ctx, "nsenter", nsenterArgs...)
	}
	cmd.Dir = "/"
	return cmd
}

func (r osCommandRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.command(ctx, name, args...).CombinedOutput()
}

func (r osCommandRunner) RunShell(ctx context.Context, label string, command string) error {
	return r.stream(ctx, func(line string) { log.Printf("%s: %s", label, line) }, "/bin/sh", "-c", command)
}

func (r osCommandRunner) Stream(ctx context.Context, onLine func(string), name string, args ...string) error {
	return r.stream(ctx, onLine, name, args...)
}

// stream runs a command with both of its output streams split into lines. The
// pipe is drained after the scan loop so a line too long to buffer stalls the
// log rather than the command that is writing it.
func (r osCommandRunner) stream(ctx context.Context, onLine func(string), name string, args ...string) error {
	cmdCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := r.command(cmdCtx, name, args...)
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	var scanning sync.WaitGroup
	scanning.Add(1)
	go func() {
		defer scanning.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(nil, maxLogLineBytes)
		for scanner.Scan() {
			if line := strings.TrimRight(scanner.Text(), "\r"); strings.TrimSpace(line) != "" {
				onLine(line)
			}
		}
		if scanner.Err() == bufio.ErrTooLong {
			cancel()
			return
		}
		_, _ = io.Copy(io.Discard, reader)
	}()

	err := cmd.Run()
	_ = writer.Close()
	scanning.Wait()
	return err
}
