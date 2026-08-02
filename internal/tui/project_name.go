package tui

import "path/filepath"

// ConfigPathProjectName derives the menu-bar project label from the absolute
// path of the resolved config file (plan 023 B3: basename of its directory).
func ConfigPathProjectName(absConfigPath string) string {
	if absConfigPath == "" {
		return ""
	}
	return dirBaseName(filepath.Dir(absConfigPath))
}

// StatusProjectName derives the menu-bar project label from GET /status
// project_dir (plan 023 B3). Empty project_dir returns "" so callers fall
// back to the cwd basename via resolveProjectName.
func StatusProjectName(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	return dirBaseName(projectDir)
}

func dirBaseName(dir string) string {
	base := filepath.Base(dir)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// resolveProjectName returns opts.ProjectName, or the cwd base as fallback (WS3).
func resolveProjectName(name string) string {
	if name != "" {
		return name
	}
	cwd, err := filepathAbs(".")
	if err != nil || cwd == "" {
		return "prox"
	}
	if base := dirBaseName(cwd); base != "" {
		return base
	}
	return "prox"
}

// filepathAbs is os.Getwd-backed; separated for tests that want to stub later.
var filepathAbs = func(path string) (string, error) {
	return filepath.Abs(path)
}
