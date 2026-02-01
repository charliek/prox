package hosts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewManager(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app", "api"})
	assert.Equal(t, "local.myapp.dev", m.domain)
	assert.Equal(t, []string{"app", "api"}, m.services)
}

func TestGetEntries(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app", "api"})
	entries := m.GetEntries()

	assert.Len(t, entries, 3)
	assert.Contains(t, entries, "local.myapp.dev")
	assert.Contains(t, entries, "app.local.myapp.dev")
	assert.Contains(t, entries, "api.local.myapp.dev")
}

func TestGenerateBlock(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app", "api"})
	block := m.generateBlock()

	assert.Contains(t, block, BlockBegin)
	assert.Contains(t, block, BlockEnd)
	assert.Contains(t, block, "127.0.0.1")
	assert.Contains(t, block, "local.myapp.dev")
	assert.Contains(t, block, "app.local.myapp.dev")
	assert.Contains(t, block, "api.local.myapp.dev")
}

func TestExtractManagedBlock(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app"})

	t.Run("extracts existing block", func(t *testing.T) {
		content := `127.0.0.1 localhost
# BEGIN prox managed block
127.0.0.1 local.myapp.dev app.local.myapp.dev
# END prox managed block
`
		block := m.extractManagedBlock(content)
		assert.Contains(t, block, BlockBegin)
		assert.Contains(t, block, BlockEnd)
		assert.Contains(t, block, "app.local.myapp.dev")
	})

	t.Run("returns empty for no block", func(t *testing.T) {
		content := "127.0.0.1 localhost\n"
		block := m.extractManagedBlock(content)
		assert.Empty(t, block)
	})

	t.Run("returns empty for incomplete block", func(t *testing.T) {
		content := `127.0.0.1 localhost
# BEGIN prox managed block
127.0.0.1 app.local.myapp.dev
`
		block := m.extractManagedBlock(content)
		assert.Empty(t, block)
	})
}

func TestRemoveManagedBlock(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app"})

	t.Run("removes existing block", func(t *testing.T) {
		content := `127.0.0.1 localhost

# BEGIN prox managed block
127.0.0.1 local.myapp.dev app.local.myapp.dev
# END prox managed block
`
		result := m.removeManagedBlock(content)
		assert.NotContains(t, result, BlockBegin)
		assert.NotContains(t, result, BlockEnd)
		assert.Contains(t, result, "127.0.0.1 localhost")
	})

	t.Run("leaves content unchanged if no block", func(t *testing.T) {
		content := "127.0.0.1 localhost\n"
		result := m.removeManagedBlock(content)
		assert.Equal(t, content, result)
	})
}

func TestAddAndRemove(t *testing.T) {
	// Create a temp hosts file
	tmpDir, err := os.MkdirTemp("", "hosts-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	hostsPath := filepath.Join(tmpDir, "hosts")
	initial := "127.0.0.1 localhost\n::1 localhost\n"
	err = os.WriteFile(hostsPath, []byte(initial), 0644)
	require.NoError(t, err)

	m := NewManagerWithPath(hostsPath, "local.myapp.dev", []string{"app", "api"})

	t.Run("Check shows no entries initially", func(t *testing.T) {
		exists, upToDate, err := m.Check()
		require.NoError(t, err)
		assert.False(t, exists)
		assert.False(t, upToDate)
	})

	t.Run("Add creates managed block", func(t *testing.T) {
		err := m.Add()
		require.NoError(t, err)

		content, err := os.ReadFile(hostsPath)
		require.NoError(t, err)

		assert.Contains(t, string(content), BlockBegin)
		assert.Contains(t, string(content), BlockEnd)
		assert.Contains(t, string(content), "app.local.myapp.dev")
		assert.Contains(t, string(content), "api.local.myapp.dev")
	})

	t.Run("Check shows entries exist and are up to date", func(t *testing.T) {
		exists, upToDate, err := m.Check()
		require.NoError(t, err)
		assert.True(t, exists)
		assert.True(t, upToDate)
	})

	t.Run("Add updates existing block", func(t *testing.T) {
		// Change services
		m.services = []string{"app", "api", "admin"}
		err := m.Add()
		require.NoError(t, err)

		content, err := os.ReadFile(hostsPath)
		require.NoError(t, err)

		assert.Contains(t, string(content), "admin.local.myapp.dev")
		// Should only have one block
		assert.Equal(t, 1, strings.Count(string(content), BlockBegin))
	})

	t.Run("Remove cleans up block", func(t *testing.T) {
		err := m.Remove()
		require.NoError(t, err)

		content, err := os.ReadFile(hostsPath)
		require.NoError(t, err)

		assert.NotContains(t, string(content), BlockBegin)
		assert.NotContains(t, string(content), BlockEnd)
		// Original content should still be there
		assert.Contains(t, string(content), "127.0.0.1 localhost")
	})
}

func TestGenerateAddCommand(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app"})
	cmd := m.GenerateAddCommand()

	assert.Contains(t, cmd, "sudo")
	assert.Contains(t, cmd, BlockBegin)
	assert.Contains(t, cmd, "app.local.myapp.dev")
}

func TestGenerateRemoveCommand(t *testing.T) {
	m := NewManager("local.myapp.dev", []string{"app"})
	cmd := m.GenerateRemoveCommand()

	assert.Contains(t, cmd, "sudo")
	assert.Contains(t, cmd, "sed")
}
