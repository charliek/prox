package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Settings holds TUI preferences persisted at ~/.prox/tui/config.toml.
type Settings struct {
	Theme        string
	ProcessPanel bool
	Timestamps   bool
	Wrap         bool
	MenuBar      bool
}

// ErrConfigUnparseable is returned by SaveSettings when the on-disk file exists
// but cannot be parsed — the file is left byte-identical (strix never-clobber
// rule, panel S4 / config.rs:117-124).
var ErrConfigUnparseable = errors.New("config file is unparseable")

// settingsPathFunc returns the settings file path. Overridable in tests
// (default: ~/.prox/tui/config.toml — prox's established user dir, not XDG;
// panel Codex #9).
var settingsPathFunc = defaultSettingsPath

func defaultSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".prox", "tui", "config.toml")
}

// DefaultSettings returns schema defaults. Defaults preserve today's behavior:
// the process panel shows, timestamps render, no soft-wrap, menu bar on.
func DefaultSettings() Settings {
	return Settings{
		ProcessPanel: true,
		Timestamps:   true,
		Wrap:         false,
		MenuBar:      true,
	}
}

// LoadSettings reads ~/.prox/tui/config.toml. A missing file yields defaults
// with no warning. A parse error yields defaults plus a warning; unknown keys
// are ignored.
func LoadSettings() (Settings, []string) {
	s := DefaultSettings()

	path := settingsPathFunc()
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, []string{fmt.Sprintf("cannot read settings: %v", err)}
	}

	var root map[string]any
	if _, err := toml.Decode(string(data), &root); err != nil {
		return s, []string{fmt.Sprintf("settings file unparseable: %v", err)}
	}

	applySettingsFromMap(&s, root)
	return s, nil
}

func applySettingsFromMap(s *Settings, root map[string]any) {
	if theme, ok := root["theme"].(string); ok {
		s.Theme = theme
	}
	view, ok := root["view"].(map[string]any)
	if !ok {
		return
	}
	if v, ok := view["process_panel"].(bool); ok {
		s.ProcessPanel = v
	}
	if v, ok := view["timestamps"].(bool); ok {
		s.Timestamps = v
	}
	if v, ok := view["wrap"].(bool); ok {
		s.Wrap = v
	}
	if v, ok := view["menu_bar"].(bool); ok {
		s.MenuBar = v
	}
}

// SaveSettings merges s into the on-disk file, preserving unknown keys.
// Comments and key ordering are lost on rewrite — BurntSushi cannot preserve
// them and a second TOML dep is not worth it (panel S4). Returns
// ErrConfigUnparseable when an existing file fails to parse; the file is never
// modified in that case.
func SaveSettings(s Settings) error {
	path := settingsPathFunc()
	if path == "" {
		return fmt.Errorf("cannot determine settings path")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	root := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		if _, err := toml.Decode(string(data), &root); err != nil {
			return fmt.Errorf("%w: %v", ErrConfigUnparseable, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	mergeSettingsIntoMap(root, s)

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		return err
	}

	return atomicWriteFile(path, buf.Bytes(), 0o644)
}

func mergeSettingsIntoMap(root map[string]any, s Settings) {
	if s.Theme != "" {
		root["theme"] = s.Theme
	}
	view, _ := root["view"].(map[string]any)
	if view == nil {
		view = make(map[string]any)
	}
	view["process_panel"] = s.ProcessPanel
	view["timestamps"] = s.Timestamps
	view["wrap"] = s.Wrap
	view["menu_bar"] = s.MenuBar
	root["view"] = view
}

// atomicWriteFile writes data to path via a unique temp file in the same
// directory (fsync, chmod, rename). The temp file is removed on any error.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
