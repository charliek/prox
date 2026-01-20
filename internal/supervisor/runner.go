// Package supervisor manages process lifecycle including starting, stopping,
// and monitoring child processes.
//
// # Security Model
//
// Commands are executed via "sh -c" to support shell features like pipes,
// redirects, and variable expansion. This means configuration files have
// the same trust level as Makefiles or Procfiles - they can execute arbitrary
// code. Only use configuration files from trusted sources.
package supervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/charliek/prox/internal/domain"
)

// ProcessRunner creates and starts processes
type ProcessRunner interface {
	Start(ctx context.Context, config domain.ProcessConfig, env map[string]string) (Process, error)
}

// Process represents a running process
type Process interface {
	PID() int
	Wait() error
	Signal(sig os.Signal) error
	Stdout() io.Reader
	Stderr() io.Reader
}

// ExecRunner implements ProcessRunner using os/exec
type ExecRunner struct{}

// NewExecRunner creates a new ExecRunner
func NewExecRunner() *ExecRunner {
	return &ExecRunner{}
}

// Start starts a new process
func (r *ExecRunner) Start(ctx context.Context, config domain.ProcessConfig, env map[string]string) (Process, error) {
	// Parse command - use shell to handle complex commands
	cmd := exec.CommandContext(ctx, "sh", "-c", config.Cmd)

	// Set up environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Create pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	// Set process group so we can kill all children
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting process: %w", err)
	}

	return &execProcess{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
	}, nil
}

// execProcess wraps exec.Cmd to implement Process interface
type execProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
}

func (p *execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}

	// Kill entire process group
	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err != nil {
		// Fall back to killing just the process
		return p.cmd.Process.Signal(sig)
	}

	return syscall.Kill(-pgid, sig.(syscall.Signal))
}

func (p *execProcess) Stdout() io.Reader {
	return p.stdout
}

func (p *execProcess) Stderr() io.Reader {
	return p.stderr
}
