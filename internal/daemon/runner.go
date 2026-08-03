package daemon

import (
	"context"
	"os"
	"os/exec"
)

type commandRunner interface {
	CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error)
	RunShell(ctx context.Context, command string) error
}

type osCommandRunner struct {
	targetPID string
}

func (r osCommandRunner) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.targetPID == "" || r.targetPID == "0" {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}

	nsenterArgs := append([]string{"-t", r.targetPID, "-m", "-u", "-i", "-n", "-p", "--", name}, args...)
	cmd := exec.CommandContext(ctx, "nsenter", nsenterArgs...)
	cmd.Dir = "/"
	return cmd.CombinedOutput()
}

func (r osCommandRunner) RunShell(ctx context.Context, command string) error {
	var cmd *exec.Cmd
	if r.targetPID == "" || r.targetPID == "0" {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	} else {
		cmd = exec.CommandContext(ctx, "nsenter", "-t", r.targetPID, "-m", "-u", "-i", "-n", "-p", "--", "/bin/sh", "-c", command)
	}
	cmd.Dir = "/"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
