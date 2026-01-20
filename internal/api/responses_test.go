package api

import (
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
)

func TestFilterSensitiveEnv(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		expected map[string]string
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name:     "empty input",
			input:    map[string]string{},
			expected: map[string]string{},
		},
		{
			name: "no sensitive vars",
			input: map[string]string{
				"PATH":     "/usr/bin",
				"HOME":     "/home/user",
				"SHELL":    "/bin/bash",
				"HOSTNAME": "localhost",
			},
			expected: map[string]string{
				"PATH":     "/usr/bin",
				"HOME":     "/home/user",
				"SHELL":    "/bin/bash",
				"HOSTNAME": "localhost",
			},
		},
		{
			name: "password variants",
			input: map[string]string{
				"PASSWORD":     "secret123",
				"DB_PASSWORD":  "dbpass",
				"MY_PASSWORD1": "mypass",
				"PASSWRD":      "notmatched", // Should NOT be redacted (doesn't contain PASSWORD)
			},
			expected: map[string]string{
				"PASSWORD":     "[REDACTED]",
				"DB_PASSWORD":  "[REDACTED]",
				"MY_PASSWORD1": "[REDACTED]",
				"PASSWRD":      "notmatched",
			},
		},
		{
			name: "secret variants",
			input: map[string]string{
				"SECRET":         "mysecret",
				"APP_SECRET":     "appsecret",
				"SECRET_KEY":     "secretkey",
				"CLIENT_SECRET":  "clientsecret",
			},
			expected: map[string]string{
				"SECRET":         "[REDACTED]",
				"APP_SECRET":     "[REDACTED]",
				"SECRET_KEY":     "[REDACTED]",
				"CLIENT_SECRET":  "[REDACTED]",
			},
		},
		{
			name: "key variants",
			input: map[string]string{
				"API_KEY":        "apikey123",
				"APIKEY":         "apikey456",
				"ACCESS_KEY":     "accesskey",
				"ACCESSKEY":      "accesskey2",
				"PRIVATE_KEY":    "privatekey",
				"SSH_KEY":        "sshkey",
				"KEYBOARD":       "notmatched", // Contains KEY but in different context
			},
			expected: map[string]string{
				"API_KEY":        "[REDACTED]",
				"APIKEY":         "[REDACTED]",
				"ACCESS_KEY":     "[REDACTED]",
				"ACCESSKEY":      "[REDACTED]",
				"PRIVATE_KEY":    "[REDACTED]",
				"SSH_KEY":        "[REDACTED]",
				"KEYBOARD":       "[REDACTED]", // Actually matches KEY pattern
			},
		},
		{
			name: "token variants",
			input: map[string]string{
				"TOKEN":          "token123",
				"AUTH_TOKEN":     "authtoken",
				"ACCESS_TOKEN":   "accesstoken",
				"REFRESH_TOKEN":  "refreshtoken",
				"GITHUB_TOKEN":   "ghtoken",
			},
			expected: map[string]string{
				"TOKEN":          "[REDACTED]",
				"AUTH_TOKEN":     "[REDACTED]",
				"ACCESS_TOKEN":   "[REDACTED]",
				"REFRESH_TOKEN":  "[REDACTED]",
				"GITHUB_TOKEN":   "[REDACTED]",
			},
		},
		{
			name: "credential variants",
			input: map[string]string{
				"CREDENTIAL":      "cred123",
				"CREDENTIALS":     "creds",
				"DB_CREDENTIAL":   "dbcred",
			},
			expected: map[string]string{
				"CREDENTIAL":      "[REDACTED]",
				"CREDENTIALS":     "[REDACTED]",
				"DB_CREDENTIAL":   "[REDACTED]",
			},
		},
		{
			name: "auth variants",
			input: map[string]string{
				"AUTH":            "auth123",
				"AUTHORIZATION":   "authz",
				"AUTH_HEADER":     "bearer xyz",
			},
			expected: map[string]string{
				"AUTH":            "[REDACTED]",
				"AUTHORIZATION":   "[REDACTED]",
				"AUTH_HEADER":     "[REDACTED]",
			},
		},
		{
			name: "private variants",
			input: map[string]string{
				"PRIVATE":         "private123",
				"PRIVATE_DATA":    "privatedata",
			},
			expected: map[string]string{
				"PRIVATE":         "[REDACTED]",
				"PRIVATE_DATA":    "[REDACTED]",
			},
		},
		{
			name: "case insensitivity",
			input: map[string]string{
				"password":       "lower",
				"Password":       "mixed",
				"PASSWORD":       "upper",
				"PaSsWoRd":       "weird",
			},
			expected: map[string]string{
				"password":       "[REDACTED]",
				"Password":       "[REDACTED]",
				"PASSWORD":       "[REDACTED]",
				"PaSsWoRd":       "[REDACTED]",
			},
		},
		{
			name: "mixed sensitive and non-sensitive",
			input: map[string]string{
				"DB_HOST":        "localhost",
				"DB_PASSWORD":    "secret",
				"API_URL":        "https://api.example.com",
				"API_KEY":        "key123",
				"LOG_LEVEL":      "debug",
			},
			expected: map[string]string{
				"DB_HOST":        "localhost",
				"DB_PASSWORD":    "[REDACTED]",
				"API_URL":        "https://api.example.com",
				"API_KEY":        "[REDACTED]",
				"LOG_LEVEL":      "debug",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterSensitiveEnv(tt.input)

			if tt.expected == nil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}

			if len(result) != len(tt.expected) {
				t.Errorf("expected length %d, got %d", len(tt.expected), len(result))
			}

			for key, expectedVal := range tt.expected {
				if gotVal, ok := result[key]; !ok {
					t.Errorf("expected key %s not found in result", key)
				} else if gotVal != expectedVal {
					t.Errorf("key %s: expected %q, got %q", key, expectedVal, gotVal)
				}
			}
		})
	}
}

func TestIsSensitiveEnvVar(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"PASSWORD", "PASSWORD", true},
		{"DB_PASSWORD", "DB_PASSWORD", true},
		{"SECRET", "SECRET", true},
		{"API_KEY", "API_KEY", true},
		{"TOKEN", "TOKEN", true},
		{"CREDENTIAL", "CREDENTIAL", true},
		{"PRIVATE", "PRIVATE", true},
		{"AUTH", "AUTH", true},
		{"APIKEY", "APIKEY", true},
		{"ACCESS_KEY", "ACCESS_KEY", true},
		{"ACCESSKEY", "ACCESSKEY", true},
		{"lowercase password", "password", true},
		{"mixed case PaSsWoRd", "PaSsWoRd", true},
		{"PATH", "PATH", false},
		{"HOME", "HOME", false},
		{"USER", "USER", false},
		{"SHELL", "SHELL", false},
		{"HOSTNAME", "HOSTNAME", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSensitiveEnvVar(tt.input)
			if result != tt.expected {
				t.Errorf("isSensitiveEnvVar(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestToProcessResponse(t *testing.T) {
	now := time.Now()
	info := domain.ProcessInfo{
		Name:         "test-process",
		State:        domain.ProcessStateRunning,
		PID:          1234,
		StartedAt:    now.Add(-10 * time.Second),
		RestartCount: 2,
		Health:       domain.HealthStatusHealthy,
	}

	resp := ToProcessResponse(info)

	if resp.Name != "test-process" {
		t.Errorf("expected Name 'test-process', got %q", resp.Name)
	}
	if resp.Status != "running" {
		t.Errorf("expected Status 'running', got %q", resp.Status)
	}
	if resp.PID != 1234 {
		t.Errorf("expected PID 1234, got %d", resp.PID)
	}
	if resp.Restarts != 2 {
		t.Errorf("expected Restarts 2, got %d", resp.Restarts)
	}
	if resp.Health != "healthy" {
		t.Errorf("expected Health 'healthy', got %q", resp.Health)
	}
	// UptimeSeconds should be approximately 10
	if resp.UptimeSeconds < 9 || resp.UptimeSeconds > 11 {
		t.Errorf("expected UptimeSeconds around 10, got %d", resp.UptimeSeconds)
	}
}

func TestToProcessDetailResponse(t *testing.T) {
	now := time.Now()
	lastCheck := now.Add(-5 * time.Second)

	info := domain.ProcessInfo{
		Name:         "test-process",
		State:        domain.ProcessStateRunning,
		PID:          1234,
		StartedAt:    now.Add(-10 * time.Second),
		RestartCount: 2,
		Health:       domain.HealthStatusHealthy,
		Cmd:          "npm start",
		Env: map[string]string{
			"NODE_ENV":    "production",
			"DB_PASSWORD": "secret123",
			"API_KEY":     "key456",
		},
		HealthDetails: &domain.HealthState{
			Enabled:             true,
			LastCheck:           lastCheck,
			LastOutput:          "OK",
			ConsecutiveFailures: 0,
		},
	}

	resp := ToProcessDetailResponse(info)

	if resp.Name != "test-process" {
		t.Errorf("expected Name 'test-process', got %q", resp.Name)
	}
	if resp.Cmd != "npm start" {
		t.Errorf("expected Cmd 'npm start', got %q", resp.Cmd)
	}

	// Check that sensitive env vars are redacted
	if resp.Env["NODE_ENV"] != "production" {
		t.Errorf("expected NODE_ENV 'production', got %q", resp.Env["NODE_ENV"])
	}
	if resp.Env["DB_PASSWORD"] != "[REDACTED]" {
		t.Errorf("expected DB_PASSWORD '[REDACTED]', got %q", resp.Env["DB_PASSWORD"])
	}
	if resp.Env["API_KEY"] != "[REDACTED]" {
		t.Errorf("expected API_KEY '[REDACTED]', got %q", resp.Env["API_KEY"])
	}

	// Check healthcheck info
	if resp.Healthcheck == nil {
		t.Fatal("expected Healthcheck to be non-nil")
	}
	if !resp.Healthcheck.Enabled {
		t.Error("expected Healthcheck.Enabled to be true")
	}
	if resp.Healthcheck.LastOutput != "OK" {
		t.Errorf("expected LastOutput 'OK', got %q", resp.Healthcheck.LastOutput)
	}
	if resp.Healthcheck.ConsecutiveFailures != 0 {
		t.Errorf("expected ConsecutiveFailures 0, got %d", resp.Healthcheck.ConsecutiveFailures)
	}
}

func TestToLogEntryResponse(t *testing.T) {
	now := time.Now()
	entry := domain.LogEntry{
		Timestamp: now,
		Process:   "web",
		Stream:    domain.StreamStdout,
		Line:      "Server started on port 3000",
	}

	resp := ToLogEntryResponse(entry)

	if resp.Process != "web" {
		t.Errorf("expected Process 'web', got %q", resp.Process)
	}
	if resp.Stream != "stdout" {
		t.Errorf("expected Stream 'stdout', got %q", resp.Stream)
	}
	if resp.Line != "Server started on port 3000" {
		t.Errorf("expected Line 'Server started on port 3000', got %q", resp.Line)
	}
	// Verify timestamp is in RFC3339Nano format
	if resp.Timestamp != now.Format(time.RFC3339Nano) {
		t.Errorf("expected Timestamp %q, got %q", now.Format(time.RFC3339Nano), resp.Timestamp)
	}
}
