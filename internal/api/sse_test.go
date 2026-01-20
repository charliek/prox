package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
)

// mockSupervisor implements the minimum interface needed for handler tests
type mockSupervisor struct{}

func (m *mockSupervisor) Status() interface{}             { return nil }
func (m *mockSupervisor) Processes() []domain.ProcessInfo { return nil }
func (m *mockSupervisor) Process(name string) (domain.ProcessInfo, error) {
	return domain.ProcessInfo{}, nil
}
func (m *mockSupervisor) StartProcess(ctx context.Context, name string) error   { return nil }
func (m *mockSupervisor) StopProcess(ctx context.Context, name string) error    { return nil }
func (m *mockSupervisor) RestartProcess(ctx context.Context, name string) error { return nil }

func TestStreamLogs_Headers(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", nil)

	// Create a request with a context that can be canceled
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run in goroutine so we can cancel it
	done := make(chan struct{})
	go func() {
		handlers.StreamLogs(rec, req)
		close(done)
	}()

	// Wait a bit for headers to be written
	time.Sleep(50 * time.Millisecond)

	// Cancel the request
	cancel()

	// Wait for handler to finish
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after context cancel")
	}

	// Check headers
	result := rec.Result()
	defer result.Body.Close()

	if ct := result.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected Content-Type 'text/event-stream', got %q", ct)
	}
	if cc := result.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control 'no-cache', got %q", cc)
	}
	if conn := result.Header.Get("Connection"); conn != "keep-alive" {
		t.Errorf("expected Connection 'keep-alive', got %q", conn)
	}
	if xab := result.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("expected X-Accel-Buffering 'no', got %q", xab)
	}
}

func TestStreamLogs_FilterParsing(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", nil)

	tests := []struct {
		name        string
		queryParams string
	}{
		{"no params", ""},
		{"process filter", "?process=web"},
		{"multiple processes", "?process=web,api"},
		{"pattern", "?pattern=error"},
		{"regex pattern", "?pattern=error.*&regex=true"},
		{"combined", "?process=web&pattern=error&regex=true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			req := httptest.NewRequest("GET", "/api/v1/logs/stream"+tt.queryParams, nil).WithContext(ctx)
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				handlers.StreamLogs(rec, req)
				close(done)
			}()

			// Wait a bit for setup
			time.Sleep(50 * time.Millisecond)

			// Cancel request
			cancel()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}

			// Should have received the connection comment
			body := rec.Body.String()
			if !strings.Contains(body, ": connected") {
				t.Errorf("expected connection comment, got %q", body)
			}
		})
	}
}

func TestStreamLogs_DataFormat(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handlers.StreamLogs(rec, req)
		close(done)
	}()

	// Wait for connection to be established
	time.Sleep(50 * time.Millisecond)

	// Write a log entry
	logMgr.Write(domain.LogEntry{
		Timestamp: time.Now(),
		Process:   "test",
		Stream:    domain.StreamStdout,
		Line:      "test message",
	})

	// Wait for it to be sent
	time.Sleep(50 * time.Millisecond)

	// Cancel request
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}

	// Parse SSE events
	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))

	foundData := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			foundData = true
			data := strings.TrimPrefix(line, "data: ")

			var entry LogEntryResponse
			if err := json.Unmarshal([]byte(data), &entry); err != nil {
				t.Errorf("failed to parse data line: %v", err)
			} else {
				if entry.Process != "test" {
					t.Errorf("expected Process 'test', got %q", entry.Process)
				}
				if entry.Stream != "stdout" {
					t.Errorf("expected Stream 'stdout', got %q", entry.Stream)
				}
				if entry.Line != "test message" {
					t.Errorf("expected Line 'test message', got %q", entry.Line)
				}
			}
		}
	}

	if !foundData {
		t.Error("expected to find data line in SSE response")
	}
}

func TestStreamLogs_InvalidPattern(t *testing.T) {
	logMgr := logs.NewManager(logs.ManagerConfig{
		BufferSize:         100,
		SubscriptionBuffer: 10,
	})
	defer logMgr.Close()

	handlers := NewHandlers(nil, logMgr, "test.yaml", nil)

	// Invalid regex pattern
	req := httptest.NewRequest("GET", "/api/v1/logs/stream?pattern=[invalid&regex=true", nil)
	rec := httptest.NewRecorder()

	handlers.StreamLogs(rec, req)

	result := rec.Result()
	defer result.Body.Close()

	if result.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", result.StatusCode)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(result.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Code != domain.ErrCodeInvalidPattern {
		t.Errorf("expected code %q, got %q", domain.ErrCodeInvalidPattern, errResp.Code)
	}
}
