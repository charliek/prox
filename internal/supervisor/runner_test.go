package supervisor

import (
	"context"
	"io"
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
