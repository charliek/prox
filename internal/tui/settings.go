package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"
)

// Settings holds TUI preferences persisted at ~/.prox/tui/config.toml.
type Settings struct {
	Theme           string
	ProcessPanel    bool
	Timestamps      bool
	Wrap            bool
	MenuBar         bool
	RequestsColumns RequestsColumns
}

// RequestsColumns controls which optional columns appear in the requests
// list (plan 023 B7 / C13). URL is always shown and is not a field here.
// Defaults are all-on (see DefaultSettings / defaultRequestsColumns).
type RequestsColumns struct {
	Time     bool
	Host     bool
	Method   bool
	Status   bool
	Duration bool
	ID       bool
}

// settingKey identifies a single persisted settings field for typed partial
// saves (plan 023 D1 / C12). Callers pass these to SaveSettingsChanged —
// never arbitrary strings.
type settingKey int

const (
	settingTheme settingKey = iota
	settingViewProcessPanel
	settingViewTimestamps
	settingViewWrap
	settingViewMenuBar
	settingRequestsColumns // [requests] table of six booleans (plan 023 C13)
)

// allSettingKeys is every currently persisted key (full-save / SaveSettings).
var allSettingKeys = []settingKey{
	settingTheme,
	settingViewProcessPanel,
	settingViewTimestamps,
	settingViewWrap,
	settingViewMenuBar,
	settingRequestsColumns,
}

// ErrConfigUnparseable is returned by SaveSettings when the on-disk file exists
// but cannot be parsed — the file is left byte-identical (strix never-clobber
// rule, panel S4 / config.rs:117-124).
var ErrConfigUnparseable = errors.New("config file is unparseable")

// errSettingsMayNotHavePersisted is returned when the rename succeeded but a
// subsequent fsync failed — the bytes are in the directory entry, but durability
// is not guaranteed (plan 023 D1).
var errSettingsMayNotHavePersisted = errors.New("settings saved but may not have persisted")

// settingsPathFunc returns the settings file path. Overridable in tests
// (default: ~/.prox/tui/config.toml — prox's established user dir, not XDG;
// panel Codex #9).
var settingsPathFunc = defaultSettingsPath

// Test seams for the flock-serialized transaction (plan 023 D1 lock-exclusion
// + fsync-failure wording). Nil in production.
var (
	settingsBeforeFlockHook func()
	settingsAfterReadHook   func()
	fsyncFileFn             = func(f *os.File) error { return f.Sync() }
)

func defaultSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".prox", "tui", "config.toml")
}

// defaultRequestsColumns returns all-on column visibility (plan 023 B7).
func defaultRequestsColumns() RequestsColumns {
	return RequestsColumns{
		Time:     true,
		Host:     true,
		Method:   true,
		Status:   true,
		Duration: true,
		ID:       true,
	}
}

// DefaultSettings returns schema defaults. Defaults preserve today's behavior:
// the process panel shows, timestamps render, no soft-wrap, menu bar on,
// all optional request columns visible.
func DefaultSettings() Settings {
	return Settings{
		ProcessPanel:    true,
		Timestamps:      true,
		Wrap:            false,
		MenuBar:         true,
		RequestsColumns: defaultRequestsColumns(),
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

	return s, applySettingsFromMap(&s, root)
}

// TOML key names, shared by the load and merge paths (CodeRabbit PR #102).
const (
	keyTheme        = "theme"
	keyView         = "view"
	keyProcessPanel = "process_panel"
	keyTimestamps   = "timestamps"
	keyWrap         = "wrap"
	keyMenuBar      = "menu_bar"
	keyRequests     = "requests"
	keyReqTime      = "time"
	keyReqHost      = "host"
	keyReqMethod    = "method"
	keyReqStatus    = "status"
	keyReqDuration  = "duration"
	keyReqID        = "id"
)

// applySettingsFromMap overlays known keys onto s. Values with the wrong
// TOML type are ignored AND reported (silently dropping `theme = 3` leaves
// the user wondering why their edit does nothing — CodeRabbit PR #102).
// A missing [requests] table leaves RequestsColumns at their all-on defaults.
func applySettingsFromMap(s *Settings, root map[string]any) []string {
	var warnings []string
	if raw, present := root[keyTheme]; present {
		if theme, ok := raw.(string); ok {
			s.Theme = theme
		} else {
			warnings = append(warnings, "theme: expected string, ignored")
		}
	}
	if rawView, present := root[keyView]; present {
		view, ok := rawView.(map[string]any)
		if !ok {
			warnings = append(warnings, "view: expected table, ignored")
		} else {
			boolKey := func(key string, dst *bool) {
				raw, present := view[key]
				if !present {
					return
				}
				if v, ok := raw.(bool); ok {
					*dst = v
				} else {
					warnings = append(warnings, key+": expected boolean, ignored")
				}
			}
			boolKey(keyProcessPanel, &s.ProcessPanel)
			boolKey(keyTimestamps, &s.Timestamps)
			boolKey(keyWrap, &s.Wrap)
			boolKey(keyMenuBar, &s.MenuBar)
		}
	}
	if rawReqs, present := root[keyRequests]; present {
		reqs, ok := rawReqs.(map[string]any)
		if !ok {
			warnings = append(warnings, "requests: expected table, ignored")
		} else {
			boolKey := func(key string, dst *bool) {
				raw, present := reqs[key]
				if !present {
					return
				}
				if v, ok := raw.(bool); ok {
					*dst = v
				} else {
					warnings = append(warnings, "requests."+key+": expected boolean, ignored")
				}
			}
			boolKey(keyReqTime, &s.RequestsColumns.Time)
			boolKey(keyReqHost, &s.RequestsColumns.Host)
			boolKey(keyReqMethod, &s.RequestsColumns.Method)
			boolKey(keyReqStatus, &s.RequestsColumns.Status)
			boolKey(keyReqDuration, &s.RequestsColumns.Duration)
			boolKey(keyReqID, &s.RequestsColumns.ID)
		}
	}
	return warnings
}

// SaveSettings writes all known keys from s (full save). Prefer
// SaveSettingsChanged at mutation call sites so concurrent writers merge
// only their changed fields.
func SaveSettings(s Settings) error {
	return SaveSettingsChanged(s, allSettingKeys...)
}

// SaveSettingsChanged runs a flock-serialized read→validate→merge→write→rename
// →fsync transaction. Only the typed keys in changed are merged from s into a
// freshly re-read on-disk map (unknown TOML keys preserved). Corrupt on-disk
// TOML refuses inside the lock and leaves the file byte-identical.
func SaveSettingsChanged(s Settings, changed ...settingKey) error {
	if len(changed) == 0 {
		return nil
	}

	path := settingsPathFunc()
	if path == "" {
		return fmt.Errorf("cannot determine settings path")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	return withSettingsLock(path, func() error {
		root := make(map[string]any)
		if data, err := os.ReadFile(path); err == nil {
			if _, err := toml.Decode(string(data), &root); err != nil {
				return fmt.Errorf("%w: %v", ErrConfigUnparseable, err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}

		if settingsAfterReadHook != nil {
			settingsAfterReadHook()
		}

		mergeChangedSettingsIntoMap(root, s, changed)

		var buf bytes.Buffer
		enc := toml.NewEncoder(&buf)
		if err := enc.Encode(root); err != nil {
			return err
		}

		return atomicWriteFile(path, buf.Bytes(), 0o644)
	})
}

// withSettingsLock serializes the settings transaction under an exclusive
// flock on a sidecar config.toml.lock (darwin/linux only — prox does not
// ship Windows).
func withSettingsLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening settings lock: %w", err)
	}
	defer f.Close()

	if settingsBeforeFlockHook != nil {
		settingsBeforeFlockHook()
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking settings: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}

func mergeChangedSettingsIntoMap(root map[string]any, s Settings, changed []settingKey) {
	var view map[string]any
	viewLoaded := false
	ensureView := func() map[string]any {
		if viewLoaded {
			return view
		}
		view, _ = root[keyView].(map[string]any)
		if view == nil {
			view = make(map[string]any)
		}
		viewLoaded = true
		return view
	}
	viewTouched := false
	for _, k := range changed {
		switch k {
		case settingTheme:
			if s.Theme != "" {
				root[keyTheme] = s.Theme
			}
		case settingViewProcessPanel:
			ensureView()[keyProcessPanel] = s.ProcessPanel
			viewTouched = true
		case settingViewTimestamps:
			ensureView()[keyTimestamps] = s.Timestamps
			viewTouched = true
		case settingViewWrap:
			ensureView()[keyWrap] = s.Wrap
			viewTouched = true
		case settingViewMenuBar:
			ensureView()[keyMenuBar] = s.MenuBar
			viewTouched = true
		case settingRequestsColumns:
			root[keyRequests] = map[string]any{
				keyReqTime:     s.RequestsColumns.Time,
				keyReqHost:     s.RequestsColumns.Host,
				keyReqMethod:   s.RequestsColumns.Method,
				keyReqStatus:   s.RequestsColumns.Status,
				keyReqDuration: s.RequestsColumns.Duration,
				keyReqID:       s.RequestsColumns.ID,
			}
		}
	}
	if viewTouched {
		root[keyView] = view
	}
}

// formatSettingsSaveError builds the footer flash text for a save failure.
// Post-rename fsync failures already say "may not have persisted"; other
// errors keep the "settings not saved:" prefix.
func formatSettingsSaveError(err error) string {
	if errors.Is(err, errSettingsMayNotHavePersisted) {
		return err.Error()
	}
	return "settings not saved: " + err.Error()
}

// atomicWriteFile writes data to path via a unique temp file in the same
// directory (fsync, chmod, rename, fsync file, fsync parent dir). The temp
// file is removed on any error before rename. Post-rename fsync failures
// return errSettingsMayNotHavePersisted.
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
	if err := fsyncFileFn(tmp); err != nil {
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

	if err := fsyncPath(path); err != nil {
		return fmt.Errorf("%w: %v", errSettingsMayNotHavePersisted, err)
	}
	if err := fsyncPath(dir); err != nil {
		return fmt.Errorf("%w: %v", errSettingsMayNotHavePersisted, err)
	}
	return nil
}

// fsyncPath opens path (file or directory) and fsyncs it.
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return fsyncFileFn(f)
}
