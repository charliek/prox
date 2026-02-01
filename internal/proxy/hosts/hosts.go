// Package hosts provides management of /etc/hosts entries for the proxy.
// It uses a managed block pattern to safely add and remove entries.
package hosts

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

const (
	// BlockBegin marks the start of prox-managed entries
	BlockBegin = "# BEGIN prox managed block"
	// BlockEnd marks the end of prox-managed entries
	BlockEnd = "# END prox managed block"
)

// Manager handles hosts file management.
type Manager struct {
	hostsPath string
	domain    string
	services  []string
}

// NewManager creates a new hosts file manager.
func NewManager(domain string, services []string) *Manager {
	return &Manager{
		hostsPath: getHostsPath(),
		domain:    domain,
		services:  services,
	}
}

// NewManagerWithPath creates a manager with a custom hosts file path (for testing).
func NewManagerWithPath(path, domain string, services []string) *Manager {
	return &Manager{
		hostsPath: path,
		domain:    domain,
		services:  services,
	}
}

// Check returns the current state of hosts entries.
// Returns (entriesExist, entriesUpToDate, error).
func (m *Manager) Check() (bool, bool, error) {
	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return false, false, fmt.Errorf("reading hosts file: %w", err)
	}

	block := m.extractManagedBlock(string(content))
	if block == "" {
		return false, false, nil
	}

	expected := m.generateBlock()
	upToDate := strings.TrimSpace(block) == strings.TrimSpace(expected)

	return true, upToDate, nil
}

// GetEntries returns the hostnames that would be added to /etc/hosts.
func (m *Manager) GetEntries() []string {
	entries := make([]string, 0, len(m.services)+1)
	// Add base domain
	entries = append(entries, m.domain)
	// Add service subdomains
	for _, svc := range m.services {
		entries = append(entries, fmt.Sprintf("%s.%s", svc, m.domain))
	}
	return entries
}

// Add adds or updates the prox managed block in the hosts file.
// This requires elevated privileges (sudo).
func (m *Manager) Add() error {
	// Get original file permissions before reading
	info, err := os.Stat(m.hostsPath)
	if err != nil {
		return fmt.Errorf("stat hosts file: %w", err)
	}
	perm := info.Mode().Perm()

	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return fmt.Errorf("reading hosts file: %w", err)
	}

	// Remove existing block if present
	newContent := m.removeManagedBlock(string(content))

	// Add new block
	block := m.generateBlock()
	newContent = strings.TrimRight(newContent, "\n") + "\n\n" + block + "\n"

	// Write back with original permissions
	if err := os.WriteFile(m.hostsPath, []byte(newContent), perm); err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}

	return nil
}

// Remove removes the prox managed block from the hosts file.
// This requires elevated privileges (sudo).
func (m *Manager) Remove() error {
	// Get original file permissions before reading
	info, err := os.Stat(m.hostsPath)
	if err != nil {
		return fmt.Errorf("stat hosts file: %w", err)
	}
	perm := info.Mode().Perm()

	content, err := os.ReadFile(m.hostsPath)
	if err != nil {
		return fmt.Errorf("reading hosts file: %w", err)
	}

	newContent := m.removeManagedBlock(string(content))

	// Clean up any extra newlines at the end
	newContent = strings.TrimRight(newContent, "\n") + "\n"

	// Write back with original permissions
	if err := os.WriteFile(m.hostsPath, []byte(newContent), perm); err != nil {
		return fmt.Errorf("writing hosts file: %w", err)
	}

	return nil
}

// generateBlock creates the managed block content.
func (m *Manager) generateBlock() string {
	entries := m.GetEntries()
	hostLine := "127.0.0.1 " + strings.Join(entries, " ")

	return fmt.Sprintf("%s\n%s\n%s", BlockBegin, hostLine, BlockEnd)
}

// extractManagedBlock returns the content of the managed block, or empty string if not found.
func (m *Manager) extractManagedBlock(content string) string {
	startIdx := strings.Index(content, BlockBegin)
	if startIdx == -1 {
		return ""
	}

	endIdx := strings.Index(content[startIdx:], BlockEnd)
	if endIdx == -1 {
		return ""
	}

	return content[startIdx : startIdx+endIdx+len(BlockEnd)]
}

// removeManagedBlock removes the managed block from the content.
func (m *Manager) removeManagedBlock(content string) string {
	startIdx := strings.Index(content, BlockBegin)
	if startIdx == -1 {
		return content
	}

	endIdx := strings.Index(content[startIdx:], BlockEnd)
	if endIdx == -1 {
		return content
	}

	// Include the newline after BlockEnd if present
	blockEnd := startIdx + endIdx + len(BlockEnd)
	if blockEnd < len(content) && content[blockEnd] == '\n' {
		blockEnd++
	}

	// Also remove the empty line before the block if present
	if startIdx > 0 && content[startIdx-1] == '\n' {
		startIdx--
	}

	return content[:startIdx] + content[blockEnd:]
}

// GenerateAddCommand returns a shell command that can be used to add hosts entries.
// This is useful when the user needs to run the command with sudo.
// The command removes any existing block first, then adds the new one.
func (m *Manager) GenerateAddCommand() string {
	entries := m.GetEntries()
	hostLine := "127.0.0.1 " + strings.Join(entries, " ")

	// Generate a command that removes existing block first, then adds new one
	removeCmd := m.GenerateRemoveCommand()
	addCmd := fmt.Sprintf(`sudo sh -c 'echo "" >> %s; echo "%s" >> %s; echo "%s" >> %s; echo "%s" >> %s'`,
		m.hostsPath,
		BlockBegin,
		m.hostsPath,
		hostLine,
		m.hostsPath,
		BlockEnd,
		m.hostsPath,
	)
	return removeCmd + " 2>/dev/null; " + addCmd
}

// GenerateRemoveCommand returns a shell command that can be used to remove hosts entries.
// The command syntax differs between macOS and Linux due to sed differences.
func (m *Manager) GenerateRemoveCommand() string {
	escapedBegin := strings.ReplaceAll(BlockBegin, " ", "\\ ")
	escapedEnd := strings.ReplaceAll(BlockEnd, " ", "\\ ")

	// macOS sed requires -i '' (empty string for in-place with no backup)
	// Linux sed uses -i.bak (backup extension)
	if runtime.GOOS == "darwin" {
		return fmt.Sprintf(`sudo sed -i '' '/%s/,/%s/d' %s`,
			escapedBegin,
			escapedEnd,
			m.hostsPath,
		)
	}
	return fmt.Sprintf(`sudo sed -i.bak '/%s/,/%s/d' %s`,
		escapedBegin,
		escapedEnd,
		m.hostsPath,
	)
}

// PrintEntries prints the entries that would be added to stdout.
func (m *Manager) PrintEntries() string {
	return m.generateBlock() + "\n"
}

// getHostsPath returns the path to the hosts file for the current OS.
func getHostsPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\drivers\etc\hosts`
	}
	return "/etc/hosts"
}
