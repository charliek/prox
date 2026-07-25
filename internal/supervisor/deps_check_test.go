package supervisor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/domain"
)

// These exercise the REAL check runners and start executor (deps_check.go)
// against live listeners, httptest servers, and real `sh -c`. They use short,
// bounded timings only.

// mkCheck builds a DependencyCheck of any kind (tcp/url/cmd) for the runner tests.
func mkCheck(kind domain.CheckKind, target string) domain.DependencyCheck {
	return domain.DependencyCheck{Kind: kind, Target: target}
}

func TestExecProberTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	p := newExecProber("", os.Environ())
	ctx := context.Background()

	if err := p.Probe(ctx, mkCheck(domain.CheckKindTCP, ln.Addr().String())); err != nil {
		t.Fatalf("live listener: got %v, want healthy", err)
	}

	// A port nobody listens on: find a free one then close it.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := free.Addr().String()
	free.Close()
	if err := p.Probe(ctx, mkCheck(domain.CheckKindTCP, refused)); err == nil {
		t.Fatalf("refused port %s: got healthy, want error", refused)
	}
}

func TestExecProberURL(t *testing.T) {
	p := newExecProber("", os.Environ())
	ctx := context.Background()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if err := p.Probe(ctx, mkCheck(domain.CheckKindURL, ok.URL)); err != nil {
		t.Fatalf("200: got %v, want healthy", err)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()
	if err := p.Probe(ctx, mkCheck(domain.CheckKindURL, fail.URL)); err == nil {
		t.Fatal("500: got healthy, want error")
	}

	// 302: redirects are NOT followed, so a raw 3xx is unhealthy even when the
	// redirect target would be 200.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound) // 302
	}))
	defer redir.Close()
	if err := p.Probe(ctx, mkCheck(domain.CheckKindURL, redir.URL)); err == nil {
		t.Fatal("302: got healthy, want error (redirects must not be followed)")
	}
}

func TestExecProberURLUntrustedTLS(t *testing.T) {
	// httptest.NewTLSServer uses a self-signed cert not in the system trust
	// store. With the default transport (system pool) the GET fails verification,
	// so the dependency is unhealthy -- documenting the trust-store behavior.
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	p := newExecProber("", os.Environ())
	if err := p.Probe(context.Background(), mkCheck(domain.CheckKindURL, ts.URL)); err == nil {
		t.Fatal("untrusted TLS: got healthy, want error (server cert not in system trust store)")
	}
}

func TestExecProberCmd(t *testing.T) {
	p := newExecProber("", os.Environ())
	ctx := context.Background()

	if err := p.Probe(ctx, mkCheck(domain.CheckKindCmd, "exit 0")); err != nil {
		t.Fatalf("exit 0: got %v, want healthy", err)
	}
	if err := p.Probe(ctx, mkCheck(domain.CheckKindCmd, "exit 1")); err == nil {
		t.Fatal("exit 1: got healthy, want error")
	}
}

func TestExecProberCmdCwdAndEnv(t *testing.T) {
	dir := t.TempDir()
	marker := "sentinel.txt"
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cwd = dir so the relative test finds the marker; env carries DEP_TOKEN.
	p := newExecProber(dir, append(os.Environ(), "DEP_TOKEN=abc123"))
	ctx := context.Background()

	if err := p.Probe(ctx, mkCheck(domain.CheckKindCmd, "test -f "+marker)); err != nil {
		t.Fatalf("cwd check: got %v, want healthy (cmd should run in the config dir)", err)
	}
	if err := p.Probe(ctx, mkCheck(domain.CheckKindCmd, `test "$DEP_TOKEN" = abc123`)); err != nil {
		t.Fatalf("env check: got %v, want healthy (env overlay should be visible)", err)
	}
}

// --- start runner -----------------------------------------------------------

func TestExecStartRunnerSuccessAndOutput(t *testing.T) {
	logs := &logCapture{}
	s := newExecStartRunner("", os.Environ(), logs.log)
	if err := s.Run(context.Background(), "db", "echo hello-from-start"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !logs.contains("dep:db: hello-from-start") {
		t.Fatalf("expected attributed output; logs: %v", logs.lines())
	}
}

func TestExecStartRunnerNonZeroExit(t *testing.T) {
	s := newExecStartRunner("", os.Environ(), func(string, ...interface{}) {})
	if err := s.Run(context.Background(), "db", "exit 3"); err == nil {
		t.Fatal("Run of `exit 3` returned nil, want non-zero exit error")
	}
}

// TestExecStartRunnerCancelKillsGroup verifies that canceling a running start
// command kills its whole process group (a grandchild spawned by the shell),
// with SIGKILL escalation after a short grace. Bounded well under 2s.
func TestExecStartRunnerCancelKillsGroup(t *testing.T) {
	s := newExecStartRunner("", os.Environ(), func(string, ...interface{}) {})
	s.grace = 200 * time.Millisecond

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "gc.pid")
	// The shell backgrounds a `sleep`, records its PID, then waits. On a group
	// kill the sleep grandchild dies too.
	cmd := fmt.Sprintf(`sleep 30 & echo $! > %s; wait`, pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, "svc", cmd) }()

	// Wait for the grandchild PID to be recorded.
	var gcPID int
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &gcPID); scanErr == nil && gcPID > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("grandchild PID never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != ctx.Err() {
			t.Logf("Run returned %v (expected context cancellation)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel within 2s")
	}

	// The grandchild sleep must be gone (group-killed), not orphaned for 30s.
	if processAlive(gcPID) {
		t.Fatalf("grandchild pid %d still alive after cancel; group kill failed", gcPID)
	}
}

func processAlive(pid int) bool {
	// Poll briefly: the SIGKILL + reap may lag the Run return by a hair.
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			return false // ESRCH: gone
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
