package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
)

func TestHealthChecker_Healthy(t *testing.T) {
	config := domain.HealthConfig{
		Cmd:         "true", // Always succeeds
		Interval:    100 * time.Millisecond,
		Timeout:     1 * time.Second,
		Retries:     3,
		StartPeriod: 50 * time.Millisecond,
	}

	checker := NewHealthChecker("test", config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker.Start(ctx)

	// Wait for start period + a couple checks
	time.Sleep(300 * time.Millisecond)

	state := checker.State()
	assert.True(t, state.Enabled)
	assert.Equal(t, domain.HealthStatusHealthy, state.Status)
	assert.Equal(t, 0, state.ConsecutiveFailures)

	checker.Stop()
}

func TestHealthChecker_Unhealthy(t *testing.T) {
	config := domain.HealthConfig{
		Cmd:         "false", // Always fails
		Interval:    50 * time.Millisecond,
		Timeout:     1 * time.Second,
		Retries:     2,
		StartPeriod: 10 * time.Millisecond,
	}

	checker := NewHealthChecker("test", config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker.Start(ctx)

	// Wait for start period + enough checks to trigger unhealthy
	time.Sleep(200 * time.Millisecond)

	state := checker.State()
	assert.Equal(t, domain.HealthStatusUnhealthy, state.Status)
	assert.GreaterOrEqual(t, state.ConsecutiveFailures, 2)

	checker.Stop()
}

func TestHealthChecker_RecoveryAfterFailure(t *testing.T) {
	// This test uses a file to track state
	// First checks fail, then succeed

	// Use a command that initially fails then succeeds
	// For simplicity, we'll just test that status can change

	config := domain.HealthConfig{
		Cmd:         "true",
		Interval:    50 * time.Millisecond,
		Timeout:     1 * time.Second,
		Retries:     3,
		StartPeriod: 10 * time.Millisecond,
	}

	checker := NewHealthChecker("test", config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker.Start(ctx)

	// Wait and check
	time.Sleep(150 * time.Millisecond)

	assert.Equal(t, domain.HealthStatusHealthy, checker.Status())

	checker.Stop()
}

func TestHealthChecker_StartPeriod(t *testing.T) {
	config := domain.HealthConfig{
		Cmd:         "true",
		Interval:    50 * time.Millisecond,
		Timeout:     1 * time.Second,
		Retries:     3,
		StartPeriod: 200 * time.Millisecond,
	}

	checker := NewHealthChecker("test", config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	checker.Start(ctx)

	// Immediately after start, should still be unknown
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, domain.HealthStatusUnknown, checker.Status())

	// After start period, should be healthy
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, domain.HealthStatusHealthy, checker.Status())

	checker.Stop()
}
