package integration

// Plan 017 C14: end-to-end coverage for the SSE data plane against the real
// binary -- heartbeats keeping idle streams alive, /processes/stream
// snapshots, /logs/stream's handshake+seq cursor contract, the no-access-log
// invariant for the control plane, and a clean shutdown while streams are
// connected. These complement the unit-level SSE tests in
// internal/api/sse_test.go (which drive handlers directly with injectable
// timings) by proving the same behavior holds through the real HTTP
// listener, real router/middleware chain, and the real (uninjectable,
// constants.SSEHeartbeatInterval-driven) production timings.
//
// Deliberately NOT covered here: an SSE client following a process or log
// stream ACROSS a daemon restart. Live (manual) verification covers that
// end-to-end, and command-level reconnect behavior (StreamID change
// detection, cursor resume) is covered by unit tests -- see
// internal/api/sse_test.go and internal/cli/client_test.go.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/prox/internal/constants"
)

// sseFrame is one parsed SSE frame: a named "event: X" data line (event set)
// or a bare "data:" line belonging to no named event (event == "").
type sseFrame struct {
	event string
	data  string
}

// sseClient connects to a real SSE endpoint and drains it continuously in the
// background, counting ": ping" heartbeat comments and collecting every
// event/data frame it sees. Tests can idle for a while (sleeping, doing other
// HTTP calls, etc.) and then inspect what arrived without babysitting a
// synchronous scan loop themselves.
type sseClient struct {
	resp *http.Response

	mu     sync.Mutex
	pings  int
	frames []sseFrame
}

// dialSSE connects to url and, on a 200 text/event-stream response, starts
// draining it in the background. It returns an error rather than failing the
// test directly so callers that must tolerate a non-200 (e.g. a
// proxy-requests stream on a config with no proxy configured) can decide what
// to do with it.
func dialSSE(url string) (*sseClient, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}

	c := &sseClient{resp: resp}
	go c.run()
	return c, nil
}

// run scans the response body line by line until it closes (client Close, or
// the server tearing the stream down), classifying each line into a ping
// count or a collected frame.
func (c *sseClient) run() {
	scanner := bufio.NewScanner(c.resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var pendingEvent string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == ": ping":
			c.mu.Lock()
			c.pings++
			c.mu.Unlock()
		case strings.HasPrefix(line, "event: "):
			pendingEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			c.mu.Lock()
			c.frames = append(c.frames, sseFrame{event: pendingEvent, data: strings.TrimPrefix(line, "data: ")})
			c.mu.Unlock()
			pendingEvent = ""
		}
	}
}

// Close disconnects the stream, ending the background scan.
func (c *sseClient) Close() {
	c.resp.Body.Close()
}

// PingCount reports how many ": ping" heartbeat comments have arrived so far.
func (c *sseClient) PingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pings
}

// Frames returns a snapshot copy of every frame collected so far.
func (c *sseClient) Frames() []sseFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sseFrame, len(c.frames))
	copy(out, c.frames)
	return out
}

// waitForFrameCountAtLeast polls c until it has collected at least n frames
// matching event, or fails the test after timeout.
func waitForFrameCountAtLeast(t *testing.T, c *sseClient, event string, n int, timeout time.Duration) []sseFrame {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var matched []sseFrame
		for _, f := range c.Frames() {
			if f.event == event {
				matched = append(matched, f)
			}
		}
		if len(matched) >= n {
			return matched
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("did not observe %d frame(s) for event %q within %v; got: %+v", n, event, timeout, c.Frames())
	return nil
}

// quietConfig renders a config with a single process that prints one
// distinctive marker on launch and then sleeps quietly (no periodic output),
// so a freshly-connected SSE client sees no log traffic at all until
// something (a restart) triggers a new line.
func quietConfig(port int) string {
	return fmt.Sprintf(`api:
  port: %d
  host: 127.0.0.1

processes:
  quiet:
    cmd: 'echo "QUIET_START"; sleep 3600'
`, port)
}

// TestAPI_SSEHeartbeatsKeepIdleStreamsAlive (plan 017 C14): idles two SSE
// connections (logs, processes) well past 2x the real heartbeat interval and
// asserts each received at least 2 ": ping" comments, then proves the logs
// stream is still fully functional afterward by restarting the process and
// observing a fresh data event. This idles ~35s, so it's guarded by
// skipShort like the other slow integration tests.
//
// The requests stream needs a proxy-enabled config; this fixture has none
// (building a dedicated proxy fixture is out of scope for this commit -- the
// requests-stream heartbeat path is unit-covered in internal/api/sse_test.go),
// so this test only confirms it correctly 503s here rather than holding it
// open.
func TestAPI_SSEHeartbeatsKeepIdleStreamsAlive(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	const port = 15565
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "prox.yaml")
	requireNoError(t, os.WriteFile(cfgPath, []byte(quietConfig(port)), 0644), "writing quiet config")

	cmd := startProx(t, binary, "up", "-c", cfgPath)
	defer killProx(cmd)

	waitForAPI(t, addr, apiReadyTimeout)
	waitForLogContains(t, addr, "quiet", "QUIET_START", 5*time.Second)

	logsClient, err := dialSSE(addr + "/api/v1/logs/stream")
	requireNoError(t, err, "connecting to logs stream")
	defer logsClient.Close()

	processesClient, err := dialSSE(addr + "/api/v1/processes/stream")
	requireNoError(t, err, "connecting to processes stream")
	defer processesClient.Close()

	// Confirm the requests stream correctly refuses without a proxy config,
	// rather than holding it open (see doc comment above).
	if resp, rerr := http.Get(addr + "/api/v1/proxy/requests/stream"); rerr == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected requests stream to 503 without a proxy config, got %d", resp.StatusCode)
		}
	} else {
		t.Errorf("GET requests stream: %v", rerr)
	}

	// Idle well past 2x the real heartbeat interval. Both streams have
	// nothing else to send (the quiet process is asleep, and no process
	// state is changing), so pings are the only thing keeping them alive.
	idleFor := 2*constants.SSEHeartbeatInterval + 5*time.Second
	time.Sleep(idleFor)

	if got := logsClient.PingCount(); got < 2 {
		t.Errorf("logs stream: expected >=2 pings after idling %v, got %d", idleFor, got)
	}
	if got := processesClient.PingCount(); got < 2 {
		t.Errorf("processes stream: expected >=2 pings after idling %v, got %d", idleFor, got)
	}

	// Now prove the logs stream still works: restarting the quiet process
	// makes it print a fresh QUIET_START line, which must arrive as a new
	// (unnamed-event) data frame.
	before := len(logsClient.Frames())
	status, errResp := restartProcess(t, addr, "quiet")
	if status != http.StatusOK {
		t.Fatalf("restart failed: status=%d code=%s error=%s", status, errResp.Code, errResp.Error)
	}
	waitForFrameCountAtLeast(t, logsClient, "", before+1, 10*time.Second)
}

// TestAPI_ProcessesStreamServesSnapshots (plan 017 C14): connects to
// /api/v1/processes/stream against the real binary and asserts the
// ": connected" preamble is followed by an initial snapshot listing the
// configured processes, then stops one process via the API and asserts a
// later snapshot converges on the stopped state (debounce coalesces bursts,
// it doesn't guarantee any particular snapshot count, so this scans forward
// with a deadline rather than asserting on the Nth event).
func TestAPI_ProcessesStreamServesSnapshots(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, apiReadyTimeout)
	waitForProcessState(t, testAPIAddr, "long", "running", 5*time.Second)

	client, err := dialSSE(testAPIAddr + "/api/v1/processes/stream")
	requireNoError(t, err, "connecting to processes stream")
	defer client.Close()

	frames := waitForFrameCountAtLeast(t, client, "", 1, 5*time.Second)

	var initial ProcessListResponse
	requireNoError(t, json.Unmarshal([]byte(frames[0].data), &initial), "decoding initial snapshot")
	names := make(map[string]bool, len(initial.Processes))
	for _, p := range initial.Processes {
		names[p.Name] = true
	}
	for _, want := range []string{"long", "echo"} {
		if !names[want] {
			t.Errorf("initial snapshot missing configured process %q: %+v", want, initial.Processes)
		}
	}

	status, errResp := stopProcess(t, testAPIAddr, "long")
	if status != http.StatusOK {
		t.Fatalf("stop failed: status=%d code=%s error=%s", status, errResp.Code, errResp.Error)
	}

	// Scan forward through whatever snapshots arrive until one shows "long"
	// stopped, or fail after a deadline. Debounce means the stop may be
	// coalesced with other changes into a single later snapshot rather than
	// producing one snapshot per transition.
	deadline := time.Now().Add(10 * time.Second)
	converged := false
	var lastSeen string
	for time.Now().Before(deadline) && !converged {
		for _, f := range client.Frames() {
			var snap ProcessListResponse
			if json.Unmarshal([]byte(f.data), &snap) != nil {
				continue
			}
			for _, p := range snap.Processes {
				if p.Name == "long" {
					lastSeen = p.Status
					if p.Status == "stopped" {
						converged = true
					}
				}
			}
		}
		if !converged {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !converged {
		t.Fatalf("no processes/stream snapshot showed 'long' stopped within deadline; last seen status: %q", lastSeen)
	}
}

// TestAPI_LogsStreamHandshakeAndCursor (plan 017 C14): connects to
// /api/v1/logs/stream and asserts the "event: handshake" frame arrives right
// after ": connected" carrying a non-empty stream_id, and that subsequent
// plain data events carry strictly-increasing non-zero seq numbers. It then
// fetches GET /api/v1/logs?since_seq=<mid> and asserts only strictly-newer
// entries come back, tagged with the same stream_id the SSE handshake
// reported.
func TestAPI_LogsStreamHandshakeAndCursor(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, apiReadyTimeout)

	client, err := dialSSE(testAPIAddr + "/api/v1/logs/stream")
	requireNoError(t, err, "connecting to logs stream")
	defer client.Close()

	handshakeFrames := waitForFrameCountAtLeast(t, client, "handshake", 1, 5*time.Second)
	var handshake struct {
		StreamID string `json:"stream_id"`
	}
	requireNoError(t, json.Unmarshal([]byte(handshakeFrames[0].data), &handshake), "decoding handshake")
	if handshake.StreamID == "" {
		t.Fatal("handshake carried an empty stream_id")
	}

	// The "long" and "echo" processes both produce steady output, so a
	// handful of plain data events should arrive quickly.
	dataFrames := waitForFrameCountAtLeast(t, client, "", 5, 10*time.Second)

	var seqs []uint64
	for _, f := range dataFrames {
		var entry struct {
			Seq uint64 `json:"seq"`
		}
		requireNoError(t, json.Unmarshal([]byte(f.data), &entry), "decoding log data frame")
		seqs = append(seqs, entry.Seq)
	}
	for i, s := range seqs {
		if s == 0 {
			t.Errorf("data frame %d carried a zero seq", i)
		}
		if i > 0 && s <= seqs[i-1] {
			t.Errorf("seq not strictly increasing at frame %d: %d -> %d", i, seqs[i-1], s)
		}
	}

	// Pick a cursor in the middle of what we've seen so far and confirm the
	// REST endpoint returns only strictly-newer entries, tagged with the same
	// stream_id.
	mid := seqs[len(seqs)/2]

	resp, err := http.Get(fmt.Sprintf("%s/api/v1/logs?since_seq=%d", testAPIAddr, mid))
	requireNoError(t, err, "GET /api/v1/logs?since_seq")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var page struct {
		Logs []struct {
			Seq uint64 `json:"seq"`
		} `json:"logs"`
		StreamID string `json:"stream_id"`
	}
	requireNoError(t, json.NewDecoder(resp.Body).Decode(&page), "decoding logs page")

	if page.StreamID != handshake.StreamID {
		t.Errorf("logs page stream_id %q does not match SSE handshake stream_id %q", page.StreamID, handshake.StreamID)
	}
	if len(page.Logs) == 0 {
		t.Fatal("expected at least one log entry newer than the mid cursor")
	}
	for _, e := range page.Logs {
		if e.Seq <= mid {
			t.Errorf("since_seq=%d returned an entry with seq %d (not strictly newer)", mid, e.Seq)
		}
	}
}

// TestUpForeground_NoControlPlaneAccessLogs (plan 017 C14): the router
// deliberately runs with no chi access-log middleware (see the doc comment on
// NewServer in internal/api/server.go) so control-plane traffic never drowns
// out `prox up`'s own process-log output on stdout/stderr. This test drives
// several real API GETs against a foreground daemon and asserts the captured
// combined output never grows a chi-style access-log line for them, while
// still containing genuine process log lines.
func TestUpForeground_NoControlPlaneAccessLogs(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	prox := startProxWithOutput(t, binary, "up", "-c", configPath("integration"))
	defer killProx(prox.cmd)

	waitForAPI(t, testAPIAddr, apiReadyTimeout)
	waitForOutputContains(t, prox, "Still running...", 5*time.Second)

	for _, path := range []string{"/api/v1/status", "/api/v1/processes", "/api/v1/logs"} {
		resp, err := http.Get(testAPIAddr + path)
		requireNoError(t, err, "GET "+path)
		resp.Body.Close()
	}

	// Give any (undesired) access-log write a moment to land before asserting
	// its absence.
	time.Sleep(500 * time.Millisecond)

	output := prox.Output()

	// chi's DefaultLogFormatter renders a request line as
	// `"GET http://host/path HTTP/1.1" from 127.0.0.1:port - 200 ...`; match
	// on the distinctive `"GET http` fragment so this fails loudly if that
	// middleware is ever reintroduced.
	if strings.Contains(output, `"GET http`) {
		t.Errorf("captured output contains a chi-style access-log line, but the control plane must not log requests:\n%s", output)
	}

	// The real process logs must still be present -- this isn't a test of an
	// empty/broken output stream.
	if !strings.Contains(output, "Still running...") {
		t.Errorf("expected genuine process log output to still be present:\n%s", output)
	}
}

// TestAPI_ShutdownPromptWithStreamsConnected (plan 017 C14): opens logs and
// processes SSE connections and holds them open, then requests a full,
// waited shutdown (POST /api/v1/shutdown?wait=true, the same idiom
// TestStopCommand_WaitsForCleanExit and friends use) and asserts the
// foreground process still exits within the existing suite's shutdown
// budget. This pins the supervisor change-bus close -> SSE teardown ->
// prompt HTTP shutdown chain: connected SSE clients must never wedge
// shutdown.
func TestAPI_ShutdownPromptWithStreamsConnected(t *testing.T) {
	skipShort(t)

	binary := buildBinary(t)
	cmd := startProx(t, binary, "up", "-c", configPath("integration"))
	// Safety net: harmless no-op once waitCmdExit below has already reaped
	// the process (Kill on an exited process and a second Wait both just
	// return discarded errors -- see waitCmdExit's doc comment) but catches
	// any earlier t.Fatal in this test before shutdown is even triggered.
	defer killProx(cmd)

	waitForAPI(t, testAPIAddr, apiReadyTimeout)
	waitForProcessState(t, testAPIAddr, "long", "running", 5*time.Second)

	logsClient, err := dialSSE(testAPIAddr + "/api/v1/logs/stream")
	requireNoError(t, err, "connecting to logs stream")
	defer logsClient.Close()

	processesClient, err := dialSSE(testAPIAddr + "/api/v1/processes/stream")
	requireNoError(t, err, "connecting to processes stream")
	defer processesClient.Close()

	// Both streams must have completed their handshake/initial snapshot
	// before we trigger shutdown, so this actually exercises live, connected
	// clients rather than ones still mid-connect.
	waitForFrameCountAtLeast(t, logsClient, "handshake", 1, 5*time.Second)
	waitForFrameCountAtLeast(t, processesClient, "", 1, 5*time.Second)

	req, err := http.NewRequest(http.MethodPost, testAPIAddr+"/api/v1/shutdown?wait=true", nil)
	requireNoError(t, err, "building shutdown request")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	requireNoError(t, err, "POST /api/v1/shutdown?wait=true")
	resp.Body.Close()
	t.Logf("waited shutdown responded after %v (status %d)", time.Since(start), resp.StatusCode)

	// Reuse the existing suite's shutdown budget (TestFullStop_NoOrphanedGrandchild
	// uses the same 20s ceiling for a full-instance stop) rather than inventing a
	// tighter one.
	if err := waitCmdExit(t, cmd, 20*time.Second); err != nil {
		t.Errorf("foreground prox up should exit cleanly with SSE clients connected, got %v", err)
	}
}
