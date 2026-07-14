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
	"errors"
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
	// GroupAlive reports whether any member of the process group is still
	// alive, using a signal-0 liveness probe. It returns (false, nil) once the
	// group is gone (ESRCH). Any surfaced error is returned alongside a
	// conservative "alive" verdict so callers never treat an ambiguous probe
	// as a confirmed reap.
	GroupAlive() (bool, error)
	Stdout() io.Reader
	Stderr() io.Reader
}

// ExecRunner implements ProcessRunner using os/exec
type ExecRunner struct{}

// NewExecRunner creates a new ExecRunner
func NewExecRunner() *ExecRunner {
	return &ExecRunner{}
}

// Start starts a new process.
// Note: The ctx parameter is accepted for interface compatibility but is not used.
// Process lifecycle is managed explicitly via Signal() to allow graceful shutdown.
// Using exec.CommandContext would send SIGKILL on context cancellation, which
// prevents processes from running their shutdown handlers.
func (r *ExecRunner) Start(ctx context.Context, config domain.ProcessConfig, env map[string]string) (Process, error) {
	_ = ctx // Explicitly mark as unused - lifecycle managed via Signal()

	cmd := exec.Command("sh", "-c", config.Cmd)

	// Set up environment
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Create manual pipes for stdout and stderr.
	// Unlike cmd.StdoutPipe(), manual pipes are NOT closed by cmd.Wait().
	// This allows grandchild processes (like uvicorn spawned by a shell)
	// to continue writing output after the shell exits.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	// Set process group so we can kill all children.
	// Note: Pdeathsig is intentionally NOT set because it would kill grandchildren
	// (like uvicorn/node) when the shell wrapper exits, preventing graceful shutdown.
	// We rely on process groups to clean up orphans instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("starting process: %w", err)
	}

	// Close write ends in parent - child process has inherited them.
	// The pipe stays open as long as ANY process holds the write end,
	// including grandchildren. This is what allows graceful shutdown
	// output to be captured.
	stdoutW.Close()
	stderrW.Close()

	// Capture the process group ID (PGID) at launch. Because SysProcAttr sets
	// Setpgid without an explicit Pgid, the child starts a brand-new process
	// group whose PGID equals the child's own PID (the child is the group
	// leader). We record the leader PID directly rather than calling
	// syscall.Getpgid: the leader PID is authoritative and this avoids a race
	// where the leader has already exited by the time we'd query it.
	return &execProcess{
		cmd:    cmd,
		pgid:   cmd.Process.Pid,
		stdout: stdoutR,
		stderr: stderrR,
	}, nil
}

// execProcess wraps exec.Cmd to implement Process interface
type execProcess struct {
	cmd    *exec.Cmd
	pgid   int // process group ID captured at launch (== leader PID); <= 0 means unknown
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
	// Signal the entire process group using the PGID captured at launch. We
	// hard-guard pgid > 0: syscall.Kill(-pgid, ...) with pgid <= 0 would signal
	// prox's own process group (pgid 0 means "my group"), which is exactly the
	// orphan/self-signal bug this replaces. We intentionally do NOT re-derive
	// the pgid via syscall.Getpgid at signal time, since after the leader exits
	// that lookup fails and we'd silently fall back to signaling nothing.
	if p.pgid > 0 {
		return syscall.Kill(-p.pgid, sig.(syscall.Signal))
	}

	// Fallback: no known group, signal the leader only if it exists.
	if p.cmd.Process != nil {
		return p.cmd.Process.Signal(sig)
	}

	return nil
}

// GroupAlive reports whether any member of the process group is still alive
// using a signal-0 liveness probe.
//
// When pgid > 0 it probes the whole group via syscall.Kill(-pgid, 0). When
// pgid <= 0 it never touches a group (that would hit prox's own process group)
// and falls back to a leader-only probe.
//
// Result mapping:
//   - err == nil                   -> (true, nil)   process/group exists
//   - errors.Is(err, ESRCH)        -> (false, nil)  process/group is gone
//   - errors.Is(err, EPERM)        -> (true, nil)   exists, not permitted to signal
//   - any other error              -> (true, err)   conservatively alive, surface err
//
// Note: a zombie leader remains a group member until it is reaped, so a probe
// can report the group as alive until monitor()'s Wait() reaps the leader.
func (p *execProcess) GroupAlive() (bool, error) {
	var err error
	if p.pgid > 0 {
		err = syscall.Kill(-p.pgid, syscall.Signal(0))
	} else {
		// No known group: probe the leader only, never a group.
		if p.cmd.Process == nil {
			return false, nil
		}
		err = p.cmd.Process.Signal(syscall.Signal(0))
	}

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, os.ErrProcessDone):
		// Reached only via the pgid<=0 leader-only fallback: os.Process.Signal
		// reports a reaped process as os.ErrProcessDone (and normalizes raw
		// ESRCH to it), not syscall.ESRCH. Treat it as gone.
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return true, err
	}
}

func (p *execProcess) Stdout() io.Reader {
	return p.stdout
}

func (p *execProcess) Stderr() io.Reader {
	return p.stderr
}
