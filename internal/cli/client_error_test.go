package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/domain"
)

// serveProcessList answers GET /api/v1/processes with names, reporting whether
// it handled the request. Stand-in daemons in this package's tests route
// through it because `prox logs` and the single-process lifecycle commands both
// consult the daemon's process list now (plan 027 C11, #95).
func serveProcessList(w http.ResponseWriter, r *http.Request, names ...string) bool {
	if r.URL.Path != "/api/v1/processes" {
		return false
	}
	resp := api.ProcessListResponse{Processes: make([]api.ProcessResponse, 0, len(names))}
	for _, n := range names {
		resp.Processes = append(resp.Processes, api.ProcessResponse{Name: n, Status: "running"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	return true
}

// processListServer is a daemon stand-in whose only job is to answer the
// process-list call.
func processListServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveProcessList(w, r, names...) {
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// --- (a) the hint fires only when it is true ---------------------------------

// TestClientError_HintTargeting is the #95 regression test at the unit level:
// the "Is prox running?" hint may be attached ONLY to an error that positively
// says nothing is listening. Every other shape — including the ones a LIVE
// daemon produces — must come back untouched, because a hint the code can
// already prove wrong is worse than no hint at all.
func TestClientError_HintTargeting(t *testing.T) {
	const hint = "Is prox running? Try 'prox up' first."

	cases := []struct {
		name     string
		err      func(t *testing.T) error
		wantHint bool
	}{
		{
			name: "4xx from a live daemon",
			err: func(t *testing.T) error {
				return statusServerError(t, http.StatusNotFound, api.ErrorResponse{
					Error: "process not found", Code: domain.ErrCodeProcessNotFound,
				})
			},
		},
		{
			name: "5xx from a live daemon",
			err: func(t *testing.T) error {
				return statusServerError(t, http.StatusInternalServerError, api.ErrorResponse{
					Error: "boom", Code: "INTERNAL_ERROR",
				})
			},
		},
		{
			name: "malformed 2xx body",
			err: func(t *testing.T) error {
				// A live daemon that answers 200 with garbage: the connection
				// was made and the response read, so nothing here says the
				// daemon is down.
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte("{not json"))
				}))
				t.Cleanup(server.Close)
				_, err := NewClientWithToken(server.URL, "").GetStatus()
				return err
			},
		},
		{
			name: "2xx whose body is cut off by a connection reset",
			err: func(t *testing.T) error {
				// The M1 case (plan 027 C16). The daemon answered 200 and then
				// the connection died mid-body, so the decode step returns an
				// error whose SHAPE is a dial failure's — ECONNRESET inside a
				// *net.OpError — even though nothing about it says the daemon is
				// down. Classification by elimination gets this wrong every
				// time; only the fact that a response was in hand settles it.
				client := NewClientWithToken("http://daemon.invalid", "")
				client.httpClient = &http.Client{Transport: resetMidBodyTransport{prefix: `{"running":`}}
				_, err := client.GetStatus()
				return err
			},
		},
		{
			name: "connection refused",
			err: func(t *testing.T) error {
				// A server that is started and immediately closed leaves a port
				// nothing is listening on — the hint's one true case.
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				addr := server.URL
				server.Close()
				_, err := NewClientWithToken(addr, "").GetStatus()
				return err
			},
			wantHint: true,
		},
		{
			name: "connection reset",
			err: func(t *testing.T) error {
				return &url.Error{
					Op:  "Get",
					URL: "http://127.0.0.1:1/api/v1/status",
					Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET},
				}
			},
			wantHint: true,
		},
		{
			name: "bare ECONNREFUSED not wrapped by http.Client",
			err: func(t *testing.T) error {
				return fmt.Errorf("dial: %w", syscall.ECONNREFUSED)
			},
			wantHint: true,
		},
		{
			name: "timeout against a listener that never answers",
			err: func(t *testing.T) error {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
				}))
				t.Cleanup(server.Close)
				client := NewClientWithToken(server.URL, "")
				client.httpClient = &http.Client{Timeout: 50 * time.Millisecond}
				_, err := client.GetStatus()
				return err
			},
		},
		{
			name: "context cancellation",
			err: func(t *testing.T) error {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
				}))
				t.Cleanup(server.Close)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := NewClientWithToken(server.URL, "").GetStatusWithContext(ctx)
				return err
			},
		},
		{
			name: "unclassifiable error fails closed",
			err: func(t *testing.T) error {
				return errors.New("something nobody anticipated")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			if err == nil {
				t.Fatal("expected the setup to produce an error")
			}
			got := clientError(err, hint)
			hinted := strings.Contains(got.Error(), hint)
			if hinted != tc.wantHint {
				t.Errorf("hint attached = %v, want %v; error was %v", hinted, tc.wantHint, got)
			}
			if !errors.Is(got, err) {
				t.Errorf("clientError must keep the original error reachable, got %v", got)
			}
		})
	}
}

// resetMidBodyTransport answers every request with a 200 whose body yields
// prefix and then fails with ECONNRESET, reproducing a daemon whose connection
// dies partway through a reply. It is a transport rather than a real socket on
// purpose: forcing a genuine RST at a chosen point of the body is a timing race
// (the reset can beat the client's header read, which produces a DIFFERENT
// error), and a test for a classification rule must not be able to land on the
// wrong input.
type resetMidBodyTransport struct{ prefix string }

func (t resetMidBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &resetAfterReader{data: []byte(t.prefix)},
		Request:    req,
	}, nil
}

// resetAfterReader hands out data, then fails the way a reset connection does.
type resetAfterReader struct {
	data []byte
	off  int
}

func (r *resetAfterReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
}

func (r *resetAfterReader) Close() error { return nil }

// TestResponseError_TagsFailuresAfterAResponse pins the positive fact the hint
// rule now rests on (plan 027 C16, M1): every error raised while reading or
// interpreting a body the daemon already sent carries *responseError, at every
// site that reads one. The hint check is a consequence — proven here too, so
// the tag and its one consumer cannot drift.
func TestResponseError_TagsFailuresAfterAResponse(t *testing.T) {
	const hint = "Is prox running? Try 'prox up' first."

	cases := []struct {
		name string
		err  func(t *testing.T) error
	}{
		{
			name: "2xx with a body that never finishes",
			err: func(t *testing.T) error {
				client := NewClientWithToken("http://daemon.invalid", "")
				client.httpClient = &http.Client{Transport: resetMidBodyTransport{prefix: `{"running":`}}
				_, err := client.GetStatus()
				return err
			},
		},
		{
			name: "2xx that is not JSON at all",
			err: func(t *testing.T) error {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte("<html>not a prox</html>"))
				}))
				t.Cleanup(server.Close)
				_, err := NewClientWithToken(server.URL, "").GetStatus()
				return err
			},
		},
		{
			name: "an SSE endpoint answering with the wrong content type",
			err: func(t *testing.T) error {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					w.Write([]byte("hello"))
				}))
				t.Cleanup(server.Close)
				_, err := NewClientWithToken(server.URL, "").StreamLogsChannel(
					context.Background(), domain.LogParams{})
				return err
			},
		},
		{
			name: "an SSE stream that hangs up mid-stream",
			err: func(t *testing.T) error {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					// Returning ends the response body: the reader's next read
					// fails, after a stream that was demonstrably established.
				}))
				t.Cleanup(server.Close)
				return NewClientWithToken(server.URL, "").ConsumeLogs(
					context.Background(), domain.LogParams{}, nil, nil,
					func(api.LogEntryResponse) {})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			if err == nil {
				t.Fatal("expected the setup to produce an error")
			}
			var respErr *responseError
			if !errors.As(err, &respErr) {
				t.Errorf("error raised after a response must carry *responseError, got %T: %v", err, err)
			}
			if got := clientError(err, hint); strings.Contains(got.Error(), hint) {
				t.Errorf("the daemon answered; the hint must not appear: %q", got.Error())
			}
		})
	}
}

// statusServerError returns the error a GetStatus against a daemon answering
// status/body produces.
func statusServerError(t *testing.T, status int, body api.ErrorResponse) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	_, err := NewClientWithToken(server.URL, "").GetStatus()
	return err
}

// TestClientError_EmptyHintPassesThrough keeps the no-hint call form honest.
func TestClientError_EmptyHintPassesThrough(t *testing.T) {
	base := errors.New("boom")
	if got := clientError(base, ""); got != base {
		t.Errorf("expected the original error, got %v", got)
	}
}

// --- (b) PROCESS_NOT_FOUND names the process ----------------------------------

func TestEnrichProcessNotFound(t *testing.T) {
	notFound := &APIError{
		Status:  http.StatusNotFound,
		Code:    domain.ErrCodeProcessNotFound,
		Message: "process not found",
	}

	cases := []struct {
		name     string
		target   string
		known    []string
		wantSubs []string
	}{
		{
			name:     "close typo suggests the one near name",
			target:   "wbe",
			known:    []string{"api", "web", "worker"},
			wantSubs: []string{`"wbe"`, `Did you mean "web"?`},
		},
		{
			name:     "wrong case suggests the exact name",
			target:   "WEB",
			known:    []string{"api", "web"},
			wantSubs: []string{`"WEB"`, `Did you mean "web"?`},
		},
		{
			name:     "nothing close lists the valid names",
			target:   "zzzzzz",
			known:    []string{"api", "web"},
			wantSubs: []string{`"zzzzzz"`, "Known processes: api, web"},
		},
		{
			name:     "an ambiguous tie lists rather than guesses",
			target:   "wer",
			known:    []string{"web", "war"},
			wantSubs: []string{"Known processes: web, war"},
		},
		{
			name:     "a daemon with no processes says so",
			target:   "web",
			known:    nil,
			wantSubs: []string{`"web"`, "This prox has no processes."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := processListServer(t, tc.known...)
			client := NewClientWithToken(server.URL, "")

			got := enrichProcessNotFound(client, tc.target, notFound)
			for _, want := range tc.wantSubs {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("expected %q in %q", want, got.Error())
				}
			}
			// The daemon's own code must survive enrichment: it is what
			// machine-readable consumers (and the tests above) classify on.
			var apiErr *APIError
			if !errors.As(got, &apiErr) || apiErr.Code != domain.ErrCodeProcessNotFound {
				t.Errorf("enrichment must keep the *APIError reachable, got %v", got)
			}
			if !strings.Contains(got.Error(), "process not found") {
				t.Errorf("enrichment must keep the daemon's message, got %q", got.Error())
			}
		})
	}
}

// TestEnrichProcessNotFound_LookupFailureKeepsOriginal pins the best-effort
// rule: an enrichment that cannot ask the daemon returns the ORIGINAL error,
// never an error of its own making.
func TestEnrichProcessNotFound_LookupFailureKeepsOriginal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	base := &APIError{Status: http.StatusNotFound, Code: domain.ErrCodeProcessNotFound, Message: "process not found"}
	got := enrichProcessNotFound(NewClientWithToken(server.URL, ""), "web", base)
	if got != error(base) {
		t.Errorf("expected the original error unchanged, got %v", got)
	}
}

// TestEnrichProcessNotFound_SlowLookupCannotDelayTheError is the M2 regression
// (plan 027 C16). Enrichment only ever IMPROVES an error the caller already
// holds, so it must not make the caller wait for it: against a daemon that
// accepts the connection and never answers, the unbounded lookup this used to
// make sat on the client's 30s budget before printing a PROCESS_NOT_FOUND that
// was decided long before.
func TestEnrichProcessNotFound_SlowLookupCannotDelayTheError(t *testing.T) {
	// Parallel: this test spends its time WAITING on processLookupTimeout, and
	// its sibling below spends the same wall clock on the same budget.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accept, then never answer
	}))
	defer server.Close()

	base := &APIError{Status: http.StatusNotFound, Code: domain.ErrCodeProcessNotFound, Message: "process not found"}

	start := time.Now()
	got := enrichProcessNotFound(NewClientWithToken(server.URL, ""), "web", base)
	elapsed := time.Since(start)

	if got != error(base) {
		t.Errorf("a lookup that timed out must return the original error, got %v", got)
	}
	// Generous headroom over processLookupTimeout: the assertion is about the
	// ORDER of magnitude (a lookup budget, not the client's 30s one).
	if bound := 3 * processLookupTimeout; elapsed > bound {
		t.Errorf("enrichment took %s, above its %s budget (bound %s)", elapsed, processLookupTimeout, bound)
	}
}

// TestValidateLogProcesses_SlowLookupFailsOpenFast is the same rule on the
// other caller of knownProcessNames: `prox logs` against an unresponsive daemon
// proceeds (fails open) without first burning the client's full timeout.
func TestValidateLogProcesses_SlowLookupFailsOpenFast(t *testing.T) {
	// Parallel: this test spends its time WAITING on processLookupTimeout, and
	// its sibling below spends the same wall clock on the same budget.
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	start := time.Now()
	err := validateLogProcesses(NewClientWithToken(server.URL, ""), "web")
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("validation must fail OPEN when the daemon cannot be asked, got %v", err)
	}
	if bound := 3 * processLookupTimeout; elapsed > bound {
		t.Errorf("validation took %s, above its %s budget (bound %s)", elapsed, processLookupTimeout, bound)
	}
}

// TestEnrichProcessNotFound_LeavesOtherErrorsAlone proves enrichment is scoped
// to the one code it can explain. PROCESS_NOT_RUNNING in particular is the
// error `prox stop <a stopped process>` returns from a LIVE daemon, and it
// already says everything true about the situation.
func TestEnrichProcessNotFound_LeavesOtherErrorsAlone(t *testing.T) {
	server := processListServer(t, "web")
	client := NewClientWithToken(server.URL, "")

	for _, base := range []error{
		&APIError{Status: http.StatusConflict, Code: domain.ErrCodeProcessNotRunning, Message: "process not running"},
		errors.New("transport exploded"),
	} {
		if got := enrichProcessNotFound(client, "web", base); got != base {
			t.Errorf("expected %v unchanged, got %v", base, got)
		}
	}
}

// TestProcessClientError_LiveDaemonGetsNoHint is the end-to-end shape of the
// original bug report: `prox stop <not running>` against a daemon that just
// answered must name the process problem and must NOT claim prox is down.
func TestProcessClientError_LiveDaemonGetsNoHint(t *testing.T) {
	server := processListServer(t, "web")
	client := NewClientWithToken(server.URL, "")

	base := &APIError{Status: http.StatusNotFound, Code: domain.ErrCodeProcessNotFound, Message: "process not found"}
	got := processClientError(client, "wbe", base, "Is prox running? Try 'prox up' first.")

	if strings.Contains(got.Error(), "Is prox running") {
		t.Errorf("the daemon answered; the hint must not appear: %q", got.Error())
	}
	if !strings.Contains(got.Error(), `Did you mean "web"?`) {
		t.Errorf("expected a suggestion, got %q", got.Error())
	}
}

func TestSuggestProcessName(t *testing.T) {
	names := []string{"web", "worker", "api"}
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		comment string
	}{
		{in: "wbe", want: "web", wantOK: true},
		{in: "workr", want: "worker", wantOK: true},
		{in: "API", want: "api", wantOK: true},
		{in: "database", wantOK: false, comment: "distance 6, nowhere near"},
		{in: "web", wantOK: false, comment: "an exact match is not a suggestion"},
	}
	for _, tc := range cases {
		got, ok := suggestProcessName(tc.in, names)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("suggestProcessName(%q) = (%q, %v), want (%q, %v) %s", tc.in, got, ok, tc.want, tc.wantOK, tc.comment)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"web", "web", 0},
		{"web", "wbe", 2},
		{"web", "wb", 1},
		{"web", "", 3},
		{"", "api", 3},
		{"kitten", "sitting", 3},
		{"café", "cafe", 1},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- (c) `prox logs <typo>` is an error, not silence ---------------------------

func TestValidateLogProcesses(t *testing.T) {
	server := processListServer(t, "web", "api")
	client := NewClientWithToken(server.URL, "")

	cases := []struct {
		name    string
		filter  string
		wantErr string // substring; empty means "must succeed"
	}{
		{name: "no filter", filter: ""},
		{name: "known name", filter: "web"},
		{name: "several known names", filter: "web,api"},
		{name: "trailing comma is harmless", filter: "web,"},
		{name: "unknown name", filter: "wbe", wantErr: `unknown process "wbe"`},
		{name: "one unknown among known", filter: "web,ap", wantErr: `unknown process "ap"`},
		{name: "wrong case is unknown", filter: "Web", wantErr: `unknown process "Web"`},
		{name: "only separators", filter: ",", wantErr: "names no process"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLogProcesses(client, tc.filter)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("expected success, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected an error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("expected %q in %q", tc.wantErr, err.Error())
			}
		})
	}
}

// TestValidateLogProcesses_FailsOpen pins the deliberate asymmetry: a daemon
// that cannot answer the process-list call must not break `prox logs`. Failing
// closed here would turn a slow daemon into a broken command, which is a worse
// regression than the silence this validation removes.
func TestValidateLogProcesses_FailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := validateLogProcesses(NewClientWithToken(server.URL, ""), "anything"); err != nil {
		t.Errorf("validation must fail open when the daemon cannot be asked, got %v", err)
	}
}

// TestRunLogs_UnknownProcessExitsNonZero covers every input path into the logs
// filter — positional, --process, comma-separated, and --follow — against the
// bug it fixes: an unmatched name used to be just a filter, so the command
// printed nothing and exited 0.
func TestRunLogs_UnknownProcessExitsNonZero(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		process  string
		follow   bool
		wantName string
	}{
		{name: "positional", args: []string{"wbe"}, wantName: "wbe"},
		{name: "flag", process: "wbe", wantName: "wbe"},
		{name: "comma separated", process: "web,wrker", wantName: "wrker"},
		{name: "follow", args: []string{"wbe"}, follow: true, wantName: "wbe"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLogsFollowFlags(t)

			var logsRequests int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveProcessList(w, r, "web", "worker") {
					return
				}
				logsRequests++
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(api.LogsResponse{})
			}))
			defer server.Close()

			apiAddr = server.URL
			logsProcess = tc.process
			logsFollow = tc.follow

			var err error
			stdout, _ := captureOutput(t, func() {
				err = runLogs(followTestCmd(context.Background()), tc.args)
			})

			if err == nil {
				t.Fatal("a mistyped process name must be an error, not silence")
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("unknown process %q", tc.wantName)) {
				t.Errorf("expected the error to name %q, got %q", tc.wantName, err.Error())
			}
			if !strings.Contains(err.Error(), "Did you mean") && !strings.Contains(err.Error(), "Known processes") {
				t.Errorf("expected the error to name a working alternative, got %q", err.Error())
			}
			if strings.Contains(err.Error(), "Is prox running") {
				t.Errorf("the daemon answered the process list; the hint must not appear: %q", err.Error())
			}
			if logsRequests != 0 {
				t.Errorf("the logs request must not be sent for an unknown name, sent %d", logsRequests)
			}
			if stdout != "" {
				t.Errorf("expected no stdout, got %q", stdout)
			}
		})
	}
}

// TestRunLogs_KnownProcessWithNoEntriesStaysQuiet pins the other half of the
// contract: silence is only an error when the NAME is wrong. A real process
// that has logged nothing still prints nothing and still exits 0.
func TestRunLogs_KnownProcessWithNoEntriesStaysQuiet(t *testing.T) {
	resetLogsFollowFlags(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveProcessList(w, r, "web") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.LogsResponse{})
	}))
	defer server.Close()

	apiAddr = server.URL
	logsFollow = false

	var err error
	stdout, _ := captureOutput(t, func() {
		err = runLogs(followTestCmd(context.Background()), []string{"web"})
	})
	if err != nil {
		t.Errorf("a known process with no entries must exit 0, got %v", err)
	}
	if stdout != "" {
		t.Errorf("expected no output, got %q", stdout)
	}
}
