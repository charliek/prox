package supervisor

import (
	"testing"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagedProcess_Config_ClonesDependsOn verifies Config() returns a deep
// copy of the DependsOn slice (plan 013 D1): mutating the returned slice must
// not leak into the live config observed by a subsequent caller.
func TestManagedProcess_Config_ClonesDependsOn(t *testing.T) {
	mp := NewManagedProcess(domain.ProcessConfig{
		Name:      "web",
		Cmd:       "./web",
		DependsOn: []string{"postgres", "redis"},
	}, nil, nil, nil)

	got := mp.Config()
	require.Equal(t, []string{"postgres", "redis"}, got.DependsOn)

	// Mutate the returned slice; the live config must be untouched.
	got.DependsOn[0] = "TAMPERED"

	again := mp.Config()
	assert.Equal(t, []string{"postgres", "redis"}, again.DependsOn, "mutation of a returned copy must not leak into the live config")
}
