package supervisor

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecRunner_Start(t *testing.T) {
	runner := NewExecRunner()

	t.Run("starts simple command", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "echo hello",
		}, nil)

		require.NoError(t, err)
		assert.Greater(t, proc.PID(), 0)

		// Read stdout
		output, err := io.ReadAll(proc.Stdout())
		require.NoError(t, err)
		assert.Contains(t, string(output), "hello")

		// Wait for completion
		err = proc.Wait()
		assert.NoError(t, err)
	})

	t.Run("passes environment", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "echo $TEST_VAR",
		}, map[string]string{"TEST_VAR": "test_value"})

		require.NoError(t, err)

		output, err := io.ReadAll(proc.Stdout())
		require.NoError(t, err)
		assert.Contains(t, string(output), "test_value")

		proc.Wait()
	})

	t.Run("captures stderr", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "echo error >&2",
		}, nil)

		require.NoError(t, err)

		output, err := io.ReadAll(proc.Stderr())
		require.NoError(t, err)
		assert.Contains(t, string(output), "error")

		proc.Wait()
	})

	t.Run("can be signaled", func(t *testing.T) {
		ctx := context.Background()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "sleep 30",
		}, nil)

		require.NoError(t, err)

		// Give it time to start
		time.Sleep(100 * time.Millisecond)

		// Send SIGTERM
		err = proc.Signal(sigterm)
		assert.NoError(t, err)

		// Wait should return
		done := make(chan error, 1)
		go func() {
			done <- proc.Wait()
		}()

		select {
		case <-done:
			// Process exited
		case <-time.After(2 * time.Second):
			t.Fatal("process did not exit after signal")
		}
	})

	t.Run("invalid command returns error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "/nonexistent/command/that/does/not/exist",
		}, nil)

		// The error might happen at Start or Wait depending on timing
		if err != nil {
			// Error at start is acceptable
			assert.Nil(t, proc)
			return
		}

		// If no error at start, wait should return an error
		err = proc.Wait()
		assert.Error(t, err)
	})

	t.Run("command exits with error code", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "exit 42",
		}, nil)

		require.NoError(t, err)

		// Wait should return an error with the exit code
		err = proc.Wait()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "42")
	})

	t.Run("captures pgid and GroupAlive flips true to false", func(t *testing.T) {
		ctx := context.Background()

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			// `exec` guarantees the shell replaces itself with sleep, so the
			// leader IS sleep (no forked shell that could leave a zombie sleep
			// reparented to init and keep the group "alive" past the deadline).
			Cmd: "exec sleep 30",
		}, nil)
		require.NoError(t, err)

		// The leader PID (== captured PGID) must be present.
		require.Greater(t, proc.PID(), 0)

		// Give it time to be fully scheduled.
		time.Sleep(100 * time.Millisecond)

		// The group is alive while the process runs.
		alive, err := proc.GroupAlive()
		require.NoError(t, err)
		assert.True(t, alive, "group should be alive while the process runs")

		// Terminate the whole group, then reap the leader. A zombie leader
		// stays a group member until reaped, so Wait() (which reaps) must run
		// before GroupAlive can flip to false.
		require.NoError(t, proc.Signal(sigkill))
		_ = proc.Wait()

		// Poll until the group is gone (ESRCH => (false, nil)).
		deadline := time.Now().Add(2 * time.Second)
		for {
			alive, err = proc.GroupAlive()
			require.NoError(t, err)
			if !alive {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("group did not become gone within deadline")
			}
			time.Sleep(20 * time.Millisecond)
		}
		assert.False(t, alive, "group should be gone after SIGKILL + reap")
	})

	t.Run("pgid<=0 never performs a group operation", func(t *testing.T) {
		// Construct an execProcess directly with pgid == 0 and an un-started
		// command (cmd.Process == nil). This must NEVER perform a group
		// operation: syscall.Kill(-0, ...) / Kill(0, ...) target the test
		// binary's OWN process group, which could kill the test runner. With a
		// nil leader process the code must take the no-op / already-gone paths.
		p := &execProcess{
			pgid: 0,
			cmd:  exec.Command("sh", "-c", "sleep 30"), // never Start()ed => Process == nil
		}
		require.Nil(t, p.cmd.Process, "guard: the command must not have been started")

		// GroupAlive with pgid<=0 and no leader process => gone, without ever
		// probing a group.
		alive, err := p.GroupAlive()
		require.NoError(t, err)
		assert.False(t, alive, "pgid<=0 with nil leader must report gone, not probe a group")

		// Signal with pgid<=0 and no leader process => no-op, never signals a group.
		require.NoError(t, p.Signal(sigkill))
		require.NoError(t, p.Signal(sigterm))

		// A negative pgid must take the same leader-only guard path (it is also
		// not > 0), never Kill(-negativePgid, ...) against some other group.
		pNeg := &execProcess{
			pgid: -1,
			cmd:  exec.Command("sh", "-c", "sleep 30"), // never Start()ed => Process == nil
		}
		require.Nil(t, pNeg.cmd.Process)
		aliveNeg, errNeg := pNeg.GroupAlive()
		require.NoError(t, errNeg)
		assert.False(t, aliveNeg, "negative pgid with nil leader must report gone, not probe a group")
		require.NoError(t, pNeg.Signal(sigkill))
	})

	t.Run("context cancellation does not kill process", func(t *testing.T) {
		// ExecRunner intentionally does NOT use exec.CommandContext so that
		// context cancellation doesn't automatically kill processes. This
		// allows for graceful shutdown via Signal() instead.
		ctx, cancel := context.WithCancel(context.Background())

		proc, err := runner.Start(ctx, domain.ProcessConfig{
			Name: "test",
			Cmd:  "sleep 30",
		}, nil)

		require.NoError(t, err)

		// Give it time to start
		time.Sleep(100 * time.Millisecond)

		// Cancel context - process should NOT be killed
		cancel()

		// Wait briefly to verify process is still running
		done := make(chan error, 1)
		go func() {
			done <- proc.Wait()
		}()

		select {
		case <-done:
			t.Fatal("process should NOT be killed by context cancellation alone")
		case <-time.After(200 * time.Millisecond):
			// Good - process is still running as expected
		}

		// Now explicitly signal the process to clean up
		proc.Signal(sigterm)
		<-done
	})
}
