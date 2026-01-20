package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/joho/godotenv"
)

// LoadEnvFile reads a .env file and returns the variables as a map
func LoadEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}

	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("env file not found: %s", path)
	}

	env, err := godotenv.Read(path)
	if err != nil {
		return nil, fmt.Errorf("reading env file %s: %w", path, err)
	}

	return env, nil
}

// MergeEnv merges multiple environment maps in order, with later maps taking precedence
func MergeEnv(envMaps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, env := range envMaps {
		for k, v := range env {
			result[k] = v
		}
	}
	return result
}

// LoadProcessEnv loads and merges environment variables for a process
// Priority (lowest to highest):
// 1. Global env_file
// 2. Process env_file
// 3. Process env variables
func LoadProcessEnv(globalEnvFile, processEnvFile string, processEnv map[string]string, configDir string) (map[string]string, error) {
	var globalEnv, procFileEnv map[string]string
	var err error

	// Load global env file
	if globalEnvFile != "" {
		envPath := resolvePath(globalEnvFile, configDir)
		globalEnv, err = LoadEnvFile(envPath)
		if err != nil {
			return nil, fmt.Errorf("loading global env file: %w", err)
		}
	}

	// Load process env file
	if processEnvFile != "" {
		envPath := resolvePath(processEnvFile, configDir)
		procFileEnv, err = LoadEnvFile(envPath)
		if err != nil {
			return nil, fmt.Errorf("loading process env file: %w", err)
		}
	}

	// Merge in order of priority
	return MergeEnv(globalEnv, procFileEnv, processEnv), nil
}

// resolvePath resolves a potentially relative path against a base directory
func resolvePath(path, baseDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if baseDir == "" {
		return path
	}
	return filepath.Join(baseDir, path)
}

// FindConfigFile searches for a config file in standard locations
func FindConfigFile() (string, error) {
	candidates := []string{
		"prox.yaml",
		"prox.yml",
		".prox.yaml",
		".prox.yml",
	}

	for _, name := range candidates {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	return "", fmt.Errorf("no config file found (tried: %v)", candidates)
}

// CheckFilePermissions checks if a file has secure permissions.
// On Unix-like systems, it verifies the file is not world-writable.
// Returns an error if the file has insecure permissions.
func CheckFilePermissions(path string) error {
	// Skip permission check on Windows
	if runtime.GOOS == "windows" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("checking file permissions: %w", err)
	}

	mode := info.Mode()

	// Check if file is world-writable (others have write permission)
	// Permission bits: rwxrwxrwx (owner, group, others)
	// World-writable = others have write (0002)
	if mode.Perm()&0002 != 0 {
		return fmt.Errorf("config file %s has insecure permissions: world-writable files can be modified by any user. Please run: chmod o-w %s", path, path)
	}

	// Also warn if group-writable, but don't fail
	// (just check, could add a warning log here if needed)

	return nil
}
