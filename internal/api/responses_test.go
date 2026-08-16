package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/supervisor"
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
				"SECRET":        "mysecret",
				"APP_SECRET":    "appsecret",
				"SECRET_KEY":    "secretkey",
				"CLIENT_SECRET": "clientsecret",
			},
			expected: map[string]string{
				"SECRET":        "[REDACTED]",
				"APP_SECRET":    "[REDACTED]",
				"SECRET_KEY":    "[REDACTED]",
				"CLIENT_SECRET": "[REDACTED]",
			},
		},
		{
			name: "key variants",
			input: map[string]string{
				"API_KEY":     "apikey123",
				"APIKEY":      "apikey456",
				"ACCESS_KEY":  "accesskey",
				"ACCESSKEY":   "accesskey2",
				"PRIVATE_KEY": "privatekey",
				"SSH_KEY":     "sshkey",
				"KEYBOARD":    "notmatched", // Contains KEY but in different context
			},
			expected: map[string]string{
				"API_KEY":     "[REDACTED]",
				"APIKEY":      "[REDACTED]",
				"ACCESS_KEY":  "[REDACTED]",
				"ACCESSKEY":   "[REDACTED]",
				"PRIVATE_KEY": "[REDACTED]",
				"SSH_KEY":     "[REDACTED]",
				"KEYBOARD":    "[REDACTED]", // Actually matches KEY pattern
			},
		},
		{
			name: "token variants",
			input: map[string]string{
				"TOKEN":         "token123",
				"AUTH_TOKEN":    "authtoken",
				"ACCESS_TOKEN":  "accesstoken",
				"REFRESH_TOKEN": "refreshtoken",
				"GITHUB_TOKEN":  "ghtoken",
			},
			expected: map[string]string{
				"TOKEN":         "[REDACTED]",
				"AUTH_TOKEN":    "[REDACTED]",
				"ACCESS_TOKEN":  "[REDACTED]",
				"REFRESH_TOKEN": "[REDACTED]",
				"GITHUB_TOKEN":  "[REDACTED]",
			},
		},
		{
			name: "credential variants",
			input: map[string]string{
				"CREDENTIAL":    "cred123",
				"CREDENTIALS":   "creds",
				"DB_CREDENTIAL": "dbcred",
			},
			expected: map[string]string{
				"CREDENTIAL":    "[REDACTED]",
				"CREDENTIALS":   "[REDACTED]",
				"DB_CREDENTIAL": "[REDACTED]",
			},
		},
		{
			name: "auth variants",
			input: map[string]string{
				"AUTH":          "auth123",
				"AUTHORIZATION": "authz",
				"AUTH_HEADER":   "bearer xyz",
			},
			expected: map[string]string{
				"AUTH":          "[REDACTED]",
				"AUTHORIZATION": "[REDACTED]",
				"AUTH_HEADER":   "[REDACTED]",
			},
		},
		{
			name: "private variants",
			input: map[string]string{
				"PRIVATE":      "private123",
				"PRIVATE_DATA": "privatedata",
			},
			expected: map[string]string{
				"PRIVATE":      "[REDACTED]",
				"PRIVATE_DATA": "[REDACTED]",
			},
		},
		{
			name: "case insensitivity",
			input: map[string]string{
				"password": "lower",
				"Password": "mixed",
				"PASSWORD": "upper",
				"PaSsWoRd": "weird",
			},
			expected: map[string]string{
				"password": "[REDACTED]",
				"Password": "[REDACTED]",
				"PASSWORD": "[REDACTED]",
				"PaSsWoRd": "[REDACTED]",
			},
		},
		{
			name: "mixed sensitive and non-sensitive",
			input: map[string]string{
				"DB_HOST":     "localhost",
				"DB_PASSWORD": "secret",
				"API_URL":     "https://api.example.com",
				"API_KEY":     "key123",
				"LOG_LEVEL":   "debug",
			},
			expected: map[string]string{
				"DB_HOST":     "localhost",
				"DB_PASSWORD": "[REDACTED]",
				"API_URL":     "https://api.example.com",
				"API_KEY":     "[REDACTED]",
				"LOG_LEVEL":   "debug",
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

// TestToProcessResponse_GatedFields pins that the waiting_on/blocked_on and kind
// fields plumb through ToProcessResponse (plan 013 D5).
func TestToProcessResponse_GatedFields(t *testing.T) {
	waiting := ToProcessResponse(domain.ProcessInfo{
		Name:      "api",
		State:     domain.ProcessStateWaiting,
		WaitingOn: []string{"postgres", "redis"},
	})
	if waiting.Status != "waiting" {
		t.Errorf("Status = %q, want waiting", waiting.Status)
	}
	if len(waiting.WaitingOn) != 2 || waiting.WaitingOn[0] != "postgres" || waiting.WaitingOn[1] != "redis" {
		t.Errorf("WaitingOn = %v, want [postgres redis]", waiting.WaitingOn)
	}
	if waiting.BlockedOn != nil {
		t.Errorf("BlockedOn = %v, want nil for a waiting process", waiting.BlockedOn)
	}

	blocked := ToProcessResponse(domain.ProcessInfo{
		Name:      "api",
		State:     domain.ProcessStateBlocked,
		Kind:      domain.ProcessKindTask,
		BlockedOn: []string{"migrate"},
	})
	if len(blocked.BlockedOn) != 1 || blocked.BlockedOn[0] != "migrate" {
		t.Errorf("BlockedOn = %v, want [migrate]", blocked.BlockedOn)
	}
	if blocked.Kind != "task" {
		t.Errorf("Kind = %q, want task", blocked.Kind)
	}
}

// TestToDependencyStatusResponse pins the check-summary formatting for the
// status Dependencies array (plan 013 D5).
func TestToDependencyStatusResponse(t *testing.T) {
	tests := []struct {
		name  string
		in    supervisor.DepStatus
		check string
	}{
		{"tcp", supervisor.DepStatus{Name: "pg", Check: domain.DependencyCheck{Kind: domain.CheckKindTCP, Target: "localhost:5435"}}, "tcp localhost:5435"},
		{"url", supervisor.DepStatus{Name: "web", Check: domain.DependencyCheck{Kind: domain.CheckKindURL, Target: "http://x/health"}}, "url http://x/health"},
		{"cmd", supervisor.DepStatus{Name: "c", Check: domain.DependencyCheck{Kind: domain.CheckKindCmd, Target: "pg_isready"}}, "cmd pg_isready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toDependencyStatusResponse(tc.in)
			if got.Check != tc.check {
				t.Errorf("Check = %q, want %q", got.Check, tc.check)
			}
			if got.Name != tc.in.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.in.Name)
			}
		})
	}
}

// TestSummarizeCheck pins the single-line collapse + truncation of check
// summaries (plan 013 D5 fix): a multi-line block-style cmd must render as one
// tabwriter-safe line, and an over-long summary is truncated with an ellipsis.
func TestSummarizeCheck(t *testing.T) {
	// Multi-line / tab-laden command collapses to one line.
	multiline := summarizeCheck("cmd", "sh -c '\n  test -f /tmp/ready\n\tsleep 1\n'")
	if strings.ContainsAny(multiline, "\n\t") {
		t.Errorf("summary still contains newlines/tabs: %q", multiline)
	}
	if strings.Contains(multiline, "  ") {
		t.Errorf("summary still contains collapsed double spaces: %q", multiline)
	}

	// Over-long summary is truncated on a rune boundary with an ellipsis, never
	// exceeding the cap.
	long := summarizeCheck("cmd", strings.Repeat("x", 200))
	if r := []rune(long); len(r) != checkSummaryMaxLen {
		t.Errorf("truncated summary len = %d runes, want %d", len(r), checkSummaryMaxLen)
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("truncated summary should end with ellipsis; got %q", long)
	}

	// Short summaries pass through unchanged.
	if got := summarizeCheck("tcp", "localhost:5435"); got != "tcp localhost:5435" {
		t.Errorf("short summary = %q, want %q", got, "tcp localhost:5435")
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
		StopTimeout:  15 * time.Second,
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

	// The effective stop budget is surfaced as a duration string.
	if resp.StopTimeout != "15s" {
		t.Errorf("expected StopTimeout '15s', got %q", resp.StopTimeout)
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

// TestToProcessDetailResponse_StopTimeoutOmittedWhenUnset verifies the
// stop_timeout field is omitted (omitempty) when the effective budget is zero
// (a process built outside the supervisor's normal resolution path), rather
// than serialized as "0s".
func TestToProcessDetailResponse_StopTimeoutOmittedWhenUnset(t *testing.T) {
	info := domain.ProcessInfo{
		Name:  "no-budget",
		State: domain.ProcessStateStopped,
		Cmd:   "true",
	}

	resp := ToProcessDetailResponse(info)
	if resp.StopTimeout != "" {
		t.Errorf("expected StopTimeout to be omitted for an unset budget, got %q", resp.StopTimeout)
	}
}

// TestHealthNoneReachesBothDTOs pins the #100 wire contract: the new "none"
// health value survives conversion into BOTH the list and detail DTOs and
// serializes as "health":"none", so a client can tell "no healthcheck
// configured" from "configured but not reporting" without guessing.
func TestHealthNoneReachesBothDTOs(t *testing.T) {
	info := domain.ProcessInfo{
		Name:   "web",
		State:  domain.ProcessStateRunning,
		PID:    4242,
		Health: domain.HealthStatusNone,
		Cmd:    "sleep 30",
		// No HealthDetails: an unconfigured check has nothing to describe.
	}

	listJSON, err := json.Marshal(ToProcessResponse(info))
	if err != nil {
		t.Fatalf("marshal list DTO: %v", err)
	}
	if !strings.Contains(string(listJSON), `"health":"none"`) {
		t.Errorf(`list DTO missing "health":"none"; got %s`, listJSON)
	}

	detail := ToProcessDetailResponse(info)
	if detail.Healthcheck != nil {
		t.Errorf("expected no healthcheck block for an unconfigured check, got %+v", detail.Healthcheck)
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail DTO: %v", err)
	}
	if !strings.Contains(string(detailJSON), `"health":"none"`) {
		t.Errorf(`detail DTO missing "health":"none"; got %s`, detailJSON)
	}
	if strings.Contains(string(detailJSON), `"healthcheck"`) {
		t.Errorf("detail DTO should omit the healthcheck block entirely; got %s", detailJSON)
	}
}

// TestHealthUnknownSurvivesForConfiguredCheck is the counterpart: a CONFIGURED
// check that has not reported still reports "unknown" (never "none"), and its
// dormant detail block carries enabled:false rather than the old hardcoded
// true.
func TestHealthUnknownSurvivesForConfiguredCheck(t *testing.T) {
	info := domain.ProcessInfo{
		Name:   "api",
		State:  domain.ProcessStateCrashed,
		Health: domain.HealthStatusUnknown,
		Cmd:    "sleep 30",
		HealthDetails: &domain.HealthState{
			Enabled: false,
			Status:  domain.HealthStatusUnknown,
		},
	}

	if got := ToProcessResponse(info).Health; got != "unknown" {
		t.Errorf("list DTO health = %q, want %q", got, "unknown")
	}

	detail := ToProcessDetailResponse(info)
	if detail.Health != "unknown" {
		t.Errorf("detail DTO health = %q, want %q", detail.Health, "unknown")
	}
	if detail.Healthcheck == nil {
		t.Fatal("expected a healthcheck block for a configured check")
	}
	if detail.Healthcheck.Enabled {
		t.Error("expected Healthcheck.Enabled false for a check whose loop is not running")
	}
}

func TestToLogEntryResponse(t *testing.T) {
	now := time.Now()
	entry := domain.LogEntry{
		Timestamp: now,
		Process:   "web",
		Stream:    domain.StreamStdout,
		Line:      "Server started on port 3000",
		Seq:       42,
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
	if resp.Seq != 42 {
		t.Errorf("expected Seq 42, got %d", resp.Seq)
	}
}

func TestToProxyRequestResponse_MapsHostname(t *testing.T) {
	now := time.Now()
	rec := proxy.RequestRecord{
		ID:         "abc1234",
		Timestamp:  now,
		Method:     "GET",
		URL:        "/api/users",
		Subdomain:  "api",
		Hostname:   "api.local.myapp.dev",
		StatusCode: 200,
		Duration:   10 * time.Millisecond,
		RemoteAddr: "127.0.0.1",
	}

	resp := ToProxyRequestResponse(rec)

	if resp.Hostname != "api.local.myapp.dev" {
		t.Errorf("expected Hostname %q, got %q", "api.local.myapp.dev", resp.Hostname)
	}
}

func TestToProxyRequestResponse_OmitsEmptyHostname(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:  "abc1234",
		URL: "/api/users",
	}

	resp := ToProxyRequestResponse(rec)

	if resp.Hostname != "" {
		t.Errorf("expected empty Hostname, got %q", resp.Hostname)
	}
}

func TestToProxyRequestResponse_MapsInFlight(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:         "abc1234",
		URL:        "/api/users",
		StatusCode: 200,
		InFlight:   true,
	}

	resp := ToProxyRequestResponse(rec)

	if !resp.InFlight {
		t.Error("expected InFlight to be true")
	}
}

func TestToProxyRequestResponse_InFlightOmittedFromJSONWhenFalse(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:  "abc1234",
		URL: "/api/users",
	}

	resp := ToProxyRequestResponse(rec)

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if strings.Contains(string(data), "in_flight") {
		t.Errorf("expected in_flight to be omitted from JSON, got %s", data)
	}
}

func TestToProxyRequestResponse_InFlightPresentInJSONWhenTrue(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:       "abc1234",
		URL:      "/api/users",
		InFlight: true,
	}

	resp := ToProxyRequestResponse(rec)

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if !strings.Contains(string(data), `"in_flight":true`) {
		t.Errorf("expected in_flight:true in JSON, got %s", data)
	}
}

// TestToProxyRequestResponse_StaleInFlight verifies D8 (#53): an in-flight
// record older than constants.InFlightStaleAfter converts with Stale true,
// and the field is present in the JSON wire format.
func TestToProxyRequestResponse_StaleInFlight(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:        "abc1234",
		URL:       "/api/stream",
		InFlight:  true,
		Timestamp: time.Now().Add(-constants.InFlightStaleAfter - time.Minute),
	}

	resp := ToProxyRequestResponse(rec)

	if !resp.Stale {
		t.Error("expected Stale to be true for an in-flight record past the staleness threshold")
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if !strings.Contains(string(data), `"stale":true`) {
		t.Errorf("expected stale:true in JSON, got %s", data)
	}
}

// TestToProxyRequestResponse_FreshInFlightNotStale verifies a recently
// started in-flight record does not convert as stale (D8, #53).
func TestToProxyRequestResponse_FreshInFlightNotStale(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:        "abc1234",
		URL:       "/api/stream",
		InFlight:  true,
		Timestamp: time.Now(),
	}

	resp := ToProxyRequestResponse(rec)

	if resp.Stale {
		t.Error("expected Stale to be false for a fresh in-flight record")
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if strings.Contains(string(data), "stale") {
		t.Errorf("expected stale to be omitted from JSON when false, got %s", data)
	}
}

// TestToProxyRequestResponse_FinalRecordNeverStale verifies a completed
// (non-in-flight) record is never reported stale regardless of age (D8, #53).
func TestToProxyRequestResponse_FinalRecordNeverStale(t *testing.T) {
	rec := proxy.RequestRecord{
		ID:         "abc1234",
		URL:        "/api/users",
		StatusCode: 200,
		Timestamp:  time.Now().Add(-24 * time.Hour),
	}

	resp := ToProxyRequestResponse(rec)

	if resp.Stale {
		t.Error("expected Stale to be false for a completed record")
	}
}
