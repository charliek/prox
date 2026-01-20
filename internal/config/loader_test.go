package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadEnvFile(t *testing.T) {
	t.Run("empty path returns nil", func(t *testing.T) {
		env, err := LoadEnvFile("")
		assert.NoError(t, err)
		assert.Nil(t, env)
	})

	t.Run("loads env file", func(t *testing.T) {
		// Create temp env file
		dir := t.TempDir()
		envPath := filepath.Join(dir, ".env")
		err := os.WriteFile(envPath, []byte("FOO=bar\nBAZ=qux"), 0644)
		require.NoError(t, err)

		env, err := LoadEnvFile(envPath)
		require.NoError(t, err)
		assert.Equal(t, "bar", env["FOO"])
		assert.Equal(t, "qux", env["BAZ"])
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := LoadEnvFile("nonexistent.env")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestMergeEnv(t *testing.T) {
	t.Run("merges multiple maps", func(t *testing.T) {
		env1 := map[string]string{"A": "1", "B": "2"}
		env2 := map[string]string{"B": "3", "C": "4"}
		env3 := map[string]string{"C": "5"}

		result := MergeEnv(env1, env2, env3)
		assert.Equal(t, "1", result["A"])
		assert.Equal(t, "3", result["B"]) // env2 overrides
		assert.Equal(t, "5", result["C"]) // env3 overrides
	})

	t.Run("handles nil maps", func(t *testing.T) {
		env1 := map[string]string{"A": "1"}
		result := MergeEnv(nil, env1, nil)
		assert.Equal(t, "1", result["A"])
	})
}

func TestLoadProcessEnv(t *testing.T) {
	dir := t.TempDir()

	// Create global env file
	globalEnv := filepath.Join(dir, ".env")
	err := os.WriteFile(globalEnv, []byte("GLOBAL=1\nSHARED=global"), 0644)
	require.NoError(t, err)

	// Create process env file
	procEnv := filepath.Join(dir, ".env.proc")
	err = os.WriteFile(procEnv, []byte("PROC=2\nSHARED=proc"), 0644)
	require.NoError(t, err)

	t.Run("merges all sources", func(t *testing.T) {
		env, err := LoadProcessEnv(".env", ".env.proc", map[string]string{
			"INLINE": "3",
			"SHARED": "inline",
		}, dir)
		require.NoError(t, err)

		assert.Equal(t, "1", env["GLOBAL"])
		assert.Equal(t, "2", env["PROC"])
		assert.Equal(t, "3", env["INLINE"])
		assert.Equal(t, "inline", env["SHARED"]) // inline wins
	})

	t.Run("handles missing global env file", func(t *testing.T) {
		_, err := LoadProcessEnv("nonexistent.env", "", nil, dir)
		require.Error(t, err)
	})
}

func TestFindConfigFile(t *testing.T) {
	// This test depends on the current directory state
	// In a clean directory, it should fail
	dir := t.TempDir()
	oldDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldDir)

	t.Run("returns error when no config found", func(t *testing.T) {
		_, err := FindConfigFile()
		require.Error(t, err)
	})

	t.Run("finds prox.yaml", func(t *testing.T) {
		err := os.WriteFile("prox.yaml", []byte("processes:\n  web: echo hi"), 0644)
		require.NoError(t, err)

		path, err := FindConfigFile()
		require.NoError(t, err)
		assert.Equal(t, "prox.yaml", path)
	})
}
