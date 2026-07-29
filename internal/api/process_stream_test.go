package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/supervisor"
)

// streamProcess is a fully controllable supervisor.Process for StreamProcesses
// tests: it dies synchronously and cleanly on SIGTERM (or SIGKILL), so
// Supervisor.StopProcess/StartProcess/RestartProcess settle -- and therefore
// notify the change bus (plan 017 C10) -- instantly and deterministically,
// with no real process spawn and no real stop_timeout wait.
type streamProcess struct {
	mu     sync.Mutex
	alive  bool
	waitCh chan struct{}
	closed bool
}

func newStreamProcess() *streamProcess {
	return &streamProcess{alive: true, waitCh: make(chan struct{})}
}

func (p *streamProcess) PID() int          { return 1 }
func (p *streamProcess) PGID() int         { return 1 }
func (p *streamProcess) StartToken() int64 { return 1 }

func (p *streamProcess) Wait() error {
	<-p.waitCh
	return nil
}

func (p *streamProcess) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
		p.mu.Lock()
		p.alive = false
		if !p.closed {
			p.closed = true
			close(p.waitCh)
		}
		p.mu.Unlock()
	}
	return nil
}

func (p *streamProcess) GroupAlive() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive, nil
}

func (p *streamProcess) Stdout() io.Reader { return strings.NewReader("") }
func (p *streamProcess) Stderr() io.Reader { return strings.NewReader("") }

// streamRunner is a supervisor.ProcessRunner that hands out streamProcess
// instances, one per Start call.
type streamRunner struct{}

func (streamRunner) Start(_ context.Context, _ domain.ProcessConfig, _ map[string]string) (supervisor.Process, error) {
	return newStreamProcess(), nil
}

// setupProcessStreamServer builds a real Supervisor (one process, "web",
// started) wired to real Handlers, using streamRunner so
// StartProcess/StopProcess/RestartProcess settle instantly and reliably
// notify the change bus without a real process or a real stop_timeout wait.
func setupProcessStreamServer(t *testing.T) (*Handlers, *supervisor.Supervisor, func()) {
	t.Helper()
	logMgr := logs.NewManager(logs.ManagerConfig{BufferSize: 100})

	cfg := &config.Config{
		API:       config.APIConfig{Port: 0, Host: "127.0.0.1"},
		Processes: map[string]config.ProcessConfig{"web": {Cmd: "unused"}},
	}
	sup := supervisor.New(cfg, logMgr, streamRunner{}, supervisor.DefaultSupervisorConfig())
	_, err := sup.Start(context.Background())
	require.NoError(t, err)

	handlers := NewHandlers(sup, logMgr, "test.yaml", nil)

	cleanup := func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sup.Stop(stopCtx) // idempotent; harmless if a test already stopped it
		logMgr.Close()
	}
	return handlers, sup, cleanup
}

// readSSEProcessSnapshot reads SSE lines from reader until it finds the next
// "data: " line and decodes it as a ProcessListResponse, skipping any ": ping"
// or other comment lines along the way. It returns an error rather than
// failing via *testing.T so it is also safe to call from a helper goroutine
// that races a test timeout (see the debounce test below).
func readSSEProcessSnapshot(reader *bufio.Reader) (ProcessListResponse, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return ProcessListResponse{}, err
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var resp ProcessListResponse
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &resp); err != nil {
			return ProcessListResponse{}, err
		}
		return resp, nil
	}
}

// requireProcessSnapshot is readSSEProcessSnapshot's *testing.T-failing
// wrapper, for tests that read on the main goroutine only (no concurrent
// timeout race).
func requireProcessSnapshot(t *testing.T, reader *bufio.Reader) ProcessListResponse {
	t.Helper()
	resp, err := readSSEProcessSnapshot(reader)
	require.NoError(t, err)
	return resp
}

// readUntilWebStatus reads snapshots from reader (via a background goroutine,
// so it is safe to race a timeout) until it sees the "web" process reach
// wantStatus or the deadline elapses, and returns the last snapshot observed
// and how many were seen. A single supervisor call can itself produce more
// than one underlying change-bus wake (e.g. a stop settles through an
// intermediate "stopping" state before its terminal "stopped" -- see
// ManagedProcess.stop), so a caller-visible "the change eventually shows up"
// assertion must tolerate one or more snapshots, not exactly one -- exactly
// the convergence guarantee the trailing-edge debounce documents (plan 017
// C11).
func readUntilWebStatus(reader *bufio.Reader, wantStatus string, timeout time.Duration) (last ProcessListResponse, seen int) {
	snapshots := make(chan ProcessListResponse, 8)
	go func() {
		for {
			snap, err := readSSEProcessSnapshot(reader)
			if err != nil {
				close(snapshots)
				return
			}
			snapshots <- snap
		}
	}()

	deadline := time.After(timeout)
	for {
		select {
		case snap, ok := <-snapshots:
			if !ok {
				return last, seen
			}
			last = snap
			seen++
			if len(last.Processes) > 0 && last.Processes[0].Status == wantStatus {
				return last, seen
			}
		case <-deadline:
			return last, seen
		}
	}
}

// TestStreamProcesses_ConnectAndInitialSnapshot asserts the connect sequence:
// ": connected", then the initial snapshot as a normal data event carrying the
// current process list (plan 017 C11).
func TestStreamProcesses_ConnectAndInitialSnapshot(t *testing.T) {
	handlers, _, cleanup := setupProcessStreamServer(t)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProcesses))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	reader := readSSEConnected(t, resp)

	snap := requireProcessSnapshot(t, reader)
	require.Len(t, snap.Processes, 1)
	require.Equal(t, "web", snap.Processes[0].Name)
	require.Equal(t, "running", snap.Processes[0].Status)
}

// TestStreamProcesses_ChangeYieldsNewSnapshot drives one process transition
// and asserts a new snapshot arrives reflecting it. A short (rather than
// zero) debounce is used deliberately: ManagedProcess.stop itself commits an
// intermediate "stopping" state (and its own change-bus wake) before its
// terminal "stopped" (see process.go), so with debounce disabled a single
// StopProcess call can legitimately surface as two snapshots ("stopping" then
// "stopped") -- both correct, undropped state, per the "every event is a full
// snapshot, no deltas" contract. The short debounce coalesces that
// same-call-stack pair into the one the caller actually wants to assert on:
// the final, settled state.
func TestStreamProcesses_ChangeYieldsNewSnapshot(t *testing.T) {
	handlers, sup, cleanup := setupProcessStreamServer(t)
	defer cleanup()
	handlers.processStreamDebounceInterval = 20 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProcesses))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)

	initial := requireProcessSnapshot(t, reader)
	require.Equal(t, "running", initial.Processes[0].Status)

	require.NoError(t, sup.StopProcess(context.Background(), "web"))

	updated, seen := readUntilWebStatus(reader, "stopped", 2*time.Second)
	require.GreaterOrEqual(t, seen, 1, "expected at least one snapshot after the change")
	require.Equal(t, "stopped", updated.Processes[0].Status)
}

// TestStreamProcesses_DebounceTrailingEdge_ConvergesOnFinalState drives a
// rapid burst of transitions (stop, start, stop) well within one debounce
// window and asserts convergence: however many snapshots the coalescing
// window yields (one, if the whole burst lands inside a single absorb window;
// occasionally two, if a wake lands just after one closes), at least one
// arrives, and the LAST one always reflects the FINAL state -- the last
// change of a burst is never lost, only ever reported on a later tick (plan
// 017 C11).
func TestStreamProcesses_DebounceTrailingEdge_ConvergesOnFinalState(t *testing.T) {
	handlers, sup, cleanup := setupProcessStreamServer(t)
	defer cleanup()
	handlers.processStreamDebounceInterval = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProcesses))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)
	_ = requireProcessSnapshot(t, reader) // initial snapshot

	// Rapid burst, well within one 50ms debounce window.
	require.NoError(t, sup.StopProcess(context.Background(), "web"))
	require.NoError(t, sup.StartProcess(context.Background(), "web"))
	require.NoError(t, sup.StopProcess(context.Background(), "web"))

	last, seen := readUntilWebStatus(reader, "stopped", 2*time.Second)
	require.GreaterOrEqual(t, seen, 1, "expected at least one snapshot after the burst")
	require.Equal(t, "stopped", last.Processes[0].Status,
		"the last snapshot observed must reflect the final (converged) state")
}

// TestStreamProcesses_Heartbeat asserts an idle stream (no process
// transitions) still emits ": ping" comments on the configured cadence. The
// initial snapshot itself is the "data" line requireSSEHeartbeats looks for
// (it carries the process name "web"), so this test needs no further data
// event beyond connect.
func TestStreamProcesses_Heartbeat(t *testing.T) {
	handlers, _, cleanup := setupProcessStreamServer(t)
	defer cleanup()
	handlers.sseHeartbeatInterval = 20 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(handlers.StreamProcesses))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)

	requireSSEHeartbeats(t, reader, "web", 3, time.Second)
}

// TestStreamProcesses_SupervisorStop_HandlerReturns asserts that when the
// supervisor stops (CloseEvents closes every change-bus subscriber channel),
// the handler observes the closed wake channel and returns promptly rather
// than hanging forever.
func TestStreamProcesses_SupervisorStop_HandlerReturns(t *testing.T) {
	handlers, sup, cleanup := setupProcessStreamServer(t)
	defer cleanup()

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.StreamProcesses(w, r)
		close(done)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	reader := readSSEConnected(t, resp)
	_ = requireProcessSnapshot(t, reader)

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, sup.Stop(stopCtx))

	requireSSEHandlerReturns(t, done, "StreamProcesses")
}

// TestStreamProcesses_NoFlusher_ReturnsJSONError mirrors
// TestStreamLogs_NoFlusher_ReturnsJSONError: a ResponseWriter that does not
// implement http.Flusher must get a clean JSON error before any SSE header is
// written, exactly like the sibling streams.
func TestStreamProcesses_NoFlusher_ReturnsJSONError(t *testing.T) {
	handlers, _, cleanup := setupProcessStreamServer(t)
	defer cleanup()

	req := httptest.NewRequest("GET", "/api/v1/processes/stream", nil)
	w := &noFlushWriter{}

	handlers.StreamProcesses(w, req)

	require.Equal(t, http.StatusInternalServerError, w.status)
	require.Equal(t, "application/json", w.Header().Get("Content-Type"))
	for _, header := range []string{"Cache-Control", "Connection", "X-Accel-Buffering"} {
		require.Empty(t, w.Header().Get(header), "expected %s to be unset on the error path", header)
	}

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(w.body, &errResp))
	require.Equal(t, domain.ErrCodeStreamingNotSupported, errResp.Code)
}

// TestStreamProcesses_ContinuousWakesCannotStarveSnapshots pins the debounce's
// max-latency bound (codex C11 finding): a wake stream that never goes quiet —
// here a tight stop/start loop against the instantly-settling streamRunner —
// must still yield snapshots, not silence.
func TestStreamProcesses_ContinuousWakesCannotStarveSnapshots(t *testing.T) {
	handlers, sup, cleanup := setupProcessStreamServer(t)
	defer cleanup()
	handlers.processStreamDebounceInterval = 30 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(handlers.StreamProcesses))
	defer server.Close()

	resp, err := http.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := readSSEConnected(t, resp)

	// Continuous churn: each stop/start settles instantly and notifies the
	// bus, restarting a pure trailing-edge debounce forever. The 5×d deadline
	// must force snapshots through regardless.
	stop := make(chan struct{})
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = sup.StopProcess(context.Background(), "web")
				_ = sup.StartProcess(context.Background(), "web")
			}
		}
	}()
	defer func() { close(stop); <-churnDone }()

	// Initial snapshot plus at least two forced ones within a bounded window
	// (deadline fires at 150ms per absorb with a 30ms debounce; 5s is ample).
	deadline := time.Now().Add(5 * time.Second)
	snapshots := 0
	for snapshots < 3 {
		require.True(t, time.Now().Before(deadline),
			"only %d snapshots under continuous churn; debounce starved the stream", snapshots)
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data: ") {
			snapshots++
		}
	}
}
