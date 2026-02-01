package certs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager(t *testing.T) {
	m := NewManager("~/.prox/certs", "local.myapp.dev")
	assert.NotNil(t, m)
	assert.Contains(t, m.certsDir, ".prox/certs")
	assert.Equal(t, "local.myapp.dev", m.domain)
}

func TestExpandPath(t *testing.T) {
	t.Run("expands tilde", func(t *testing.T) {
		home, _ := os.UserHomeDir()
		result := expandPath("~/foo/bar")
		assert.Equal(t, filepath.Join(home, "foo/bar"), result)
	})

	t.Run("leaves absolute path unchanged", func(t *testing.T) {
		result := expandPath("/absolute/path")
		assert.Equal(t, "/absolute/path", result)
	})

	t.Run("leaves relative path unchanged", func(t *testing.T) {
		result := expandPath("relative/path")
		assert.Equal(t, "relative/path", result)
	})
}

func TestGetCertPaths(t *testing.T) {
	m := NewManager("/tmp/certs", "local.myapp.dev")
	paths := m.GetCertPaths()

	assert.Equal(t, "/tmp/certs/local_myapp_dev.pem", paths.CertFile)
	assert.Equal(t, "/tmp/certs/local_myapp_dev-key.pem", paths.KeyFile)
}

func TestCertsExist(t *testing.T) {
	// Create a temp directory for test certs
	tmpDir, err := os.MkdirTemp("", "certs-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	m := NewManager(tmpDir, "test.dev")
	paths := m.getCertPaths()

	t.Run("returns false when no certs exist", func(t *testing.T) {
		assert.False(t, m.certsExist(paths))
	})

	t.Run("returns false when only cert exists", func(t *testing.T) {
		err := os.WriteFile(paths.CertFile, []byte("cert"), 0600)
		require.NoError(t, err)
		assert.False(t, m.certsExist(paths))
		os.Remove(paths.CertFile)
	})

	t.Run("returns false when only key exists", func(t *testing.T) {
		err := os.WriteFile(paths.KeyFile, []byte("key"), 0600)
		require.NoError(t, err)
		assert.False(t, m.certsExist(paths))
		os.Remove(paths.KeyFile)
	})

	t.Run("returns true when both exist", func(t *testing.T) {
		err := os.WriteFile(paths.CertFile, []byte("cert"), 0600)
		require.NoError(t, err)
		err = os.WriteFile(paths.KeyFile, []byte("key"), 0600)
		require.NoError(t, err)
		assert.True(t, m.certsExist(paths))
	})
}

func TestCheckMkcert(t *testing.T) {
	m := NewManager("/tmp/certs", "test.dev")

	// This test depends on whether mkcert is installed
	// We just verify the function doesn't panic
	err := m.CheckMkcert()
	if err != nil {
		assert.Contains(t, err.Error(), "mkcert")
	}
}
