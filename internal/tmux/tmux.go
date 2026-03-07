package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const socket = "muxac"

// Runner is the interface for tmux operations.
type Runner interface {
	HasSession(ctx context.Context, sessionName string) bool
	AttachSession(ctx context.Context, sessionName string) error
	NewSession(ctx context.Context, sessionName string, env []string, command string, sourceFile string) error
	ListSessionNames(ctx context.Context) ([]string, error)
	KillSession(ctx context.Context, sessionName string) error
	NewDetachedSession(ctx context.Context, sessionName string, command string) error
}

// ExecRunner implements Runner by executing the tmux binary.
type ExecRunner struct{}

func (r *ExecRunner) HasSession(ctx context.Context, sessionName string) bool {
	args := []string{"-L", socket, "has-session", "-t", "=" + sessionName}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	return cmd.Run() == nil
}

func (r *ExecRunner) AttachSession(ctx context.Context, sessionName string) error {
	args := []string{"-L", socket, "attach-session", "-t", "=" + sessionName}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *ExecRunner) NewSession(ctx context.Context, sessionName string, env []string, command string, sourceFile string) error {
	args := []string{"-L", socket, "new-session", "-s", sessionName}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, command)
	if sourceFile != "" {
		args = append(args, ";", "source-file", sourceFile)
	}

	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Run()
}

func (r *ExecRunner) KillSession(ctx context.Context, sessionName string) error {
	args := []string{"-L", socket, "kill-session", "-t", "=" + sessionName}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	return cmd.Run()
}

func (r *ExecRunner) NewDetachedSession(ctx context.Context, sessionName string, command string) error {
	cmd := exec.CommandContext(ctx, "tmux", "-L", socket, "new-session", "-d", "-s", sessionName, command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Run()
}

func (r *ExecRunner) ListSessionNames(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "tmux", "-L", socket, "list-sessions", "-F", "#{session_name}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// tmux returns error when no sessions exist — treat as empty.
		return nil, nil
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}
