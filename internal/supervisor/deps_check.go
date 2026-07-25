package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// This file holds the REAL readiness-probe runners (tcp/url/cmd) and the real
// start-command executor for the dependency resolver (plan 013 D2). They sit
// behind the Prober and StartRunner interfaces in deps.go so unit tests script
// outcomes; these implementations get their own focused tests against live
// listeners, httptest servers, and real `sh -c`.

// startKillGrace is how long a canceled start: command's process group is given
// to exit after SIGTERM before it is SIGKILLed (plan 013 D2). It mirrors the
// process-stop escalation shape; kept modest so cancellation converges quickly.
const startKillGrace = constants.KillGrace

// execProber implements Prober with the real tcp/url/cmd check runners.
type execProber struct {
	cwd string
	env []string
	// client is reused across url checks. Its CheckRedirect returns
	// ErrUseLastResponse so a 3xx is delivered as a raw response (NOT followed)
	// and therefore NOT treated as healthy -- only a 2xx is. Transport is left
	// nil so it uses http.DefaultTransport, i.e. the system trust store for TLS.
	client *http.Client
}

func newExecProber(cwd string, env []string) *execProber {
	return &execProber{
		cwd: cwd,
		env: env,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Probe dispatches to the runner for the check's kind. The per-attempt deadline
// is carried by ctx (the resolver cancels it); every runner honors ctx.
func (p *execProber) Probe(ctx context.Context, check domain.DependencyCheck) error {
	switch check.Kind {
	case domain.CheckKindTCP:
		return p.probeTCP(ctx, check.Target)
	case domain.CheckKindURL:
		return p.probeURL(ctx, check.Target)
	case domain.CheckKindCmd:
		return p.probeCmd(ctx, check.Target)
	default:
		return fmt.Errorf("unknown check kind %q", check.Kind)
	}
}

// probeTCP dials the target address; a successful connect is healthy. It uses
// DialContext (not net.DialTimeout) so the resolver's per-attempt context --
// including the injected clock's cancellation -- bounds the dial; functionally
// this is the same "dial succeeds => ready" check.
func (p *execProber) probeTCP(ctx context.Context, target string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return fmt.Errorf("tcp dial %s: %w", target, err)
	}
	return conn.Close()
}

// probeURL issues a GET and treats ONLY a 2xx as healthy. Redirects are NOT
// followed (CheckRedirect on the client), so a raw 3xx is unhealthy. The
// response body is always drained and closed.
func (p *execProber) probeURL(ctx context.Context, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("url check %s: %w", target, err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("url check %s: %w", target, err)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused. The 4096-byte cap is a deliberate
	// trade-off: readiness endpoints return tiny bodies, and bounding the drain
	// protects the per-attempt budget against a large or slow body. At worst this
	// forgoes keep-alive reuse for an oversized response (the unread remainder
	// closes the connection), which is fine for a periodic readiness probe. ctx
	// still applies to the read.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("url check %s: status %d", target, resp.StatusCode)
}

// probeCmd runs the check command via `sh -c`; exit 0 is healthy. It mirrors the
// healthcheck executor (health.go): output is captured for the last-error
// surface, cwd/env match the process execution context. ctx cancellation kills
// the command (exec.CommandContext).
func (p *execProber) probeCmd(ctx context.Context, cmd string) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = p.cwd
	c.Env = p.env
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	if err := c.Run(); err != nil {
		out := strings.TrimSpace(buf.String())
		if len(out) > 500 {
			out = out[:500] + "..."
		}
		if out != "" {
			return fmt.Errorf("cmd check failed: %w (output: %s)", err, out)
		}
		return fmt.Errorf("cmd check failed: %w", err)
	}
	return nil
}

// execStartRunner implements StartRunner: it runs a dependency's start: command
// once in its OWN process group, streams output to the system log attributed as
// "dep:<name>", and on cancellation kills the whole group (SIGTERM, then SIGKILL
// after startKillGrace).
type execStartRunner struct {
	cwd   string
	env   []string
	log   LogFunc
	grace time.Duration
}

func newExecStartRunner(cwd string, env []string, log LogFunc) *execStartRunner {
	return &execStartRunner{cwd: cwd, env: env, log: log, grace: startKillGrace}
}

// Run launches the start command and blocks until it exits or ctx is canceled.
// On cancellation the command's process group is signaled (SIGTERM, escalating
// to SIGKILL after grace) so grandchildren spawned by the shell are cleaned up
// too. It returns the command's exit error, or ctx.Err() when canceled.
func (s *execStartRunner) Run(ctx context.Context, name, cmd string) error {
	// Check for cancellation BEFORE launching anything (Fix 7): a generation that
	// was reset/closed between demand and here must not spawn a side-effecting
	// process (e.g. `docker compose up`) that we would immediately have to kill.
	if err := ctx.Err(); err != nil {
		return err
	}

	c := exec.Command("sh", "-c", cmd)
	c.Dir = s.cwd
	c.Env = s.env
	// Own process group so a group kill reaches shell-spawned grandchildren
	// without touching prox's own group (mirrors runner.go).
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dep %s start: stdout pipe: %w", name, err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return fmt.Errorf("dep %s start: stderr pipe: %w", name, err)
	}

	if err := c.Start(); err != nil {
		return fmt.Errorf("dep %s start: %w", name, err)
	}
	// Group leader PID == PGID (Setpgid with no explicit Pgid), captured now
	// while the PID definitively names this child.
	pgid := c.Process.Pid

	var streams sync.WaitGroup
	streams.Add(2)
	go s.pump(&streams, name, stdout)
	go s.pump(&streams, name, stderr)

	waitCh := make(chan error, 1)
	go func() { waitCh <- c.Wait() }()

	select {
	case err := <-waitCh:
		streams.Wait()
		return err
	case <-ctx.Done():
		s.killGroup(pgid)
		<-waitCh
		streams.Wait()
		return ctx.Err()
	}
}

// pump streams one output pipe to the system log, one line per entry, attributed
// to the dependency.
func (s *execStartRunner) pump(wg *sync.WaitGroup, name string, r io.Reader) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, constants.ScannerBufferSize), constants.ScannerMaxBufferSize)
	for scanner.Scan() {
		s.log("dep:%s: %s", name, scanner.Text())
	}
}

// killGroup signals the start command's process group: SIGTERM, then SIGKILL
// after grace if it has not exited. The pgid>0 guard mirrors runner.go: a
// non-positive pgid with syscall.Kill(-pgid, ...) would hit prox's own group.
func (s *execStartRunner) killGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	// This escalation uses the REAL clock deliberately, not the resolver's Clock
	// seam (Doc 8): it only ever runs against real OS processes. Unit tests inject
	// a fake StartRunner and never reach this path, so there is nothing to
	// virtualize here; the real runner's own cancellation test exercises it with a
	// short injected grace.
	deadline := time.Now().Add(s.grace)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := syscall.Kill(-pgid, syscall.Signal(0)); err != nil {
			// Group is gone.
			return
		}
		if !time.Now().Before(deadline) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		<-ticker.C
	}
}
