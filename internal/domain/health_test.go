package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHealthStatus_String(t *testing.T) {
	assert.Equal(t, "healthy", HealthStatusHealthy.String())
	assert.Equal(t, "unhealthy", HealthStatusUnhealthy.String())
	assert.Equal(t, "unknown", HealthStatusUnknown.String())
}

func TestHealthConfig_WithDefaults(t *testing.T) {
	t.Run("applies all defaults", func(t *testing.T) {
		config := HealthConfig{Cmd: "curl http://localhost/health"}
		result := config.WithDefaults()

		assert.Equal(t, "curl http://localhost/health", result.Cmd)
		assert.Equal(t, 10*time.Second, result.Interval)
		assert.Equal(t, 5*time.Second, result.Timeout)
		assert.Equal(t, 3, result.Retries)
		assert.Equal(t, 30*time.Second, result.StartPeriod)
	})

	t.Run("preserves existing values", func(t *testing.T) {
		config := HealthConfig{
			Cmd:         "curl http://localhost/health",
			Interval:    5 * time.Second,
			Timeout:     2 * time.Second,
			Retries:     5,
			StartPeriod: 60 * time.Second,
		}
		result := config.WithDefaults()

		assert.Equal(t, 5*time.Second, result.Interval)
		assert.Equal(t, 2*time.Second, result.Timeout)
		assert.Equal(t, 5, result.Retries)
		assert.Equal(t, 60*time.Second, result.StartPeriod)
	})
}
