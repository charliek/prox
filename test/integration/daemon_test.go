package integration

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDaemonMode_StartsInBackground(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create a simple config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "while true; do echo hello; sleep 1; done"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	cmd := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Output should indicate daemon started
	if !strings.Contains(string(output), "prox started (pid") {
		t.Errorf("expected daemon start message, got: %s", output)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Read state file to get port
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Verify API is accessible
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	waitForAPI(t, apiAddr, 10*time.Second)

	// Clean up: stop the daemon
	stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
	resp, err := http.DefaultClient.Do(stopReq)
	if err == nil && resp != nil {
		resp.Body.Close()
	}

	// Wait for cleanup
	time.Sleep(500 * time.Millisecond)
}

func TestDaemonMode_CreatesStateFile(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create a simple config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	cmd := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Verify state file exists and read it
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	var state struct {
		PID        int    `json:"pid"`
		Port       int    `json:"port"`
		Host       string `json:"host"`
		ConfigFile string `json:"config_file"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	if state.PID == 0 {
		t.Error("state PID is 0")
	}
	if state.Port == 0 {
		t.Error("state Port is 0")
	}
	if state.Host == "" {
		t.Error("state Host is empty")
	}

	// Verify PID file exists
	pidPath := filepath.Join(tmpDir, ".prox", "prox.pid")
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("PID file not found: %v", err)
	}

	pidStr := strings.TrimSpace(string(pidData))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("invalid PID in file: %v", err)
	}

	if pid != state.PID {
		t.Errorf("PID mismatch: file has %d, state has %d", pid, state.PID)
	}

	// Clean up
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
	resp, err := http.DefaultClient.Do(stopReq)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)
}

func TestDaemonMode_RejectsSecondInstance(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create a simple config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start first daemon
	cmd1 := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd1.Dir = tmpDir
	output1, err := cmd1.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start first daemon: %v\noutput: %s", err, output1)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Try to start second daemon - should fail
	cmd2 := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd2.Dir = tmpDir
	output2, err := cmd2.CombinedOutput()

	// Should fail
	if err == nil {
		t.Fatalf("expected second daemon to fail, but it succeeded\noutput: %s", output2)
	}

	// Should mention already running
	if !strings.Contains(string(output2), "already running") {
		t.Errorf("expected 'already running' error, got: %s", output2)
	}

	// Clean up: read state and stop
	stateData, _ := os.ReadFile(filepath.Join(tmpDir, ".prox", "prox.state"))
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Logf("warning: failed to parse state for cleanup: %v", err)
	}
	if state.Port > 0 {
		apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
		stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
		resp, err := http.DefaultClient.Do(stopReq)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}
	time.Sleep(500 * time.Millisecond)
}

func TestDaemonMode_GracefulShutdown(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create a simple config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	cmd := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Read state
	stateData, err := os.ReadFile(filepath.Join(tmpDir, ".prox", "prox.state"))
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Stop daemon
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
	resp, err := http.DefaultClient.Do(stopReq)
	if err != nil {
		t.Fatalf("failed to send shutdown request: %v", err)
	}
	resp.Body.Close()

	// Wait for shutdown - poll for state file removal
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); os.IsNotExist(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify state file is cleaned up
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should have been removed after shutdown")
	}

	// Verify PID file is cleaned up
	pidPath := filepath.Join(tmpDir, ".prox", "prox.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should have been removed after shutdown")
	}
}

func TestDaemonMode_DynamicPort(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config WITHOUT api.port - should use dynamic port
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	cmd := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Read state
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Port should be assigned dynamically (not the default 5555)
	if state.Port == 0 {
		t.Error("expected dynamic port to be assigned")
	}

	// Verify API is accessible on the dynamic port
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	waitForAPI(t, apiAddr, 10*time.Second)

	t.Logf("Daemon using dynamic port: %d", state.Port)

	// Clean up
	stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
	resp, err := http.DefaultClient.Do(stopReq)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)
}

func TestDaemonMode_ConfiguredPort(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config WITH specific api.port
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
api:
  port: 16666
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	cmd := exec.Command(binary, "up", "-d", "-c", configPath)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Read state
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	// Port should be the configured value
	if state.Port != 16666 {
		t.Errorf("expected port 16666, got %d", state.Port)
	}

	// Verify API is accessible on the configured port
	apiAddr := "http://127.0.0.1:16666"
	waitForAPI(t, apiAddr, 10*time.Second)

	// Clean up
	stopReq, _ := http.NewRequest("POST", apiAddr+"/api/v1/shutdown", nil)
	resp, err := http.DefaultClient.Do(stopReq)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)
}

func TestDaemonMode_CLIAutoDiscovery(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start daemon
	startCmd := exec.Command(binary, "up", "-d", "-c", configPath)
	startCmd.Dir = tmpDir
	output, err := startCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to start daemon: %v\noutput: %s", err, output)
	}

	// Wait for state file to contain valid port (retry for partial writes)
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	var state struct {
		Port int `json:"port"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stateData, err := os.ReadFile(statePath)
		if err == nil {
			if json.Unmarshal(stateData, &state) == nil && state.Port != 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if state.Port == 0 {
		t.Fatalf("failed to get valid port from state file")
	}

	// Wait for API to be ready before running CLI command
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	waitForAPI(t, apiAddr, 10*time.Second)

	// Run status command without specifying --addr
	// It should auto-discover the API address from .prox/prox.state
	statusCmd := exec.Command(binary, "status", "-c", configPath)
	statusCmd.Dir = tmpDir
	statusOutput, err := statusCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status command failed: %v\noutput: %s", err, statusOutput)
	}

	// Should show running status
	if !strings.Contains(string(statusOutput), "running") {
		t.Errorf("expected 'running' in status output, got: %s", statusOutput)
	}

	// Clean up - also using auto-discovery
	stopCmd := exec.Command(binary, "stop", "-c", configPath)
	stopCmd.Dir = tmpDir
	stopCmd.CombinedOutput()
	time.Sleep(500 * time.Millisecond)
}

func TestUpCommand_ForegroundDynamicPort(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config WITHOUT api.port - should use dynamic port
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start foreground mode (no -d flag)
	cmd := exec.Command(binary, "up", "-c", configPath)
	cmd.Dir = tmpDir
	cmd.Start()
	defer killProx(cmd)

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Read state file to verify dynamic port was written
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file in foreground mode: %v", err)
	}

	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("failed to parse state: %v", err)
	}

	if state.Port == 0 {
		t.Error("expected dynamic port to be assigned in foreground mode")
	}

	// Verify API is accessible
	apiAddr := "http://127.0.0.1:" + strconv.Itoa(state.Port)
	waitForAPI(t, apiAddr, 10*time.Second)

	t.Logf("Foreground mode using dynamic port: %d", state.Port)
}

func TestUpCommand_ForegroundCreatesStateFile(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
api:
  port: 16667
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start foreground mode
	cmd := exec.Command(binary, "up", "-c", configPath)
	cmd.Dir = tmpDir
	cmd.Start()
	defer killProx(cmd)

	// Wait for state file to be created
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Verify state file exists
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatal("state file not created in foreground mode")
	}

	// Verify PID file exists
	pidPath := filepath.Join(tmpDir, ".prox", "prox.pid")
	if _, err := os.Stat(pidPath); os.IsNotExist(err) {
		t.Fatal("PID file not created in foreground mode")
	}
}

func TestUpCommand_ForegroundRejectsSecondInstance(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	tmpDir := t.TempDir()

	// Create config
	configPath := filepath.Join(tmpDir, "prox.yaml")
	err := os.WriteFile(configPath, []byte(`
api:
  port: 16668
processes:
  test: "sleep 60"
`), 0644)
	if err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// Start first instance
	cmd1 := exec.Command(binary, "up", "-c", configPath)
	cmd1.Dir = tmpDir
	cmd1.Start()
	defer killProx(cmd1)

	// Wait for state file to be created (indicates PID file is also locked)
	statePath := filepath.Join(tmpDir, ".prox", "prox.state")
	waitForStateFile(t, statePath, 10*time.Second)

	// Try to start second instance - should fail
	cmd2 := exec.Command(binary, "up", "-c", configPath)
	cmd2.Dir = tmpDir
	output, err := cmd2.CombinedOutput()

	if err == nil {
		t.Fatalf("expected second instance to fail, but it succeeded\noutput: %s", output)
	}

	// Should mention already running
	if !strings.Contains(string(output), "already running") {
		t.Errorf("expected 'already running' error, got: %s", output)
	}
}
