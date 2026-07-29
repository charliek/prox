package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/logs"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
	"github.com/charliek/prox/internal/supervisor"
)

// Mode represents the current TUI mode
type Mode int

const (
	ModeNormal Mode = iota
	ModeFilter
	ModeSearch
	ModeStringFilter
	ModeHelp
)

// ViewMode represents which content is being displayed
type ViewMode int

const (
	ViewModeLogs ViewMode = iota
	ViewModeRequests
	ViewModeRequestDetail
)

// Model is the bubbletea model for the TUI
type Model struct {
	BaseModel

	// Dependencies
	supervisor *supervisor.Supervisor
	logManager *logs.Manager

	// Subscription ID for log tracking
	subID string
}

// NewModel creates a new TUI model
func NewModel(sup *supervisor.Supervisor, logMgr *logs.Manager) Model {
	base := newBaseModel(HelpConfig{
		TitleSuffix: "",
		QuitMessage: "Quit",
	})

	// Initialize filter to show all processes
	for _, p := range sup.Processes() {
		base.filterProcesses[p.Name] = true
	}
	base.processes = sup.Processes()

	// Local mode reads in-process subscriptions: every stream is healthy from
	// the start and only ever changes if a subscription channel closes under it.
	for _, id := range allStreams {
		base.streamHealth[id] = stream.Status{State: stream.StateOK}
	}

	return Model{
		BaseModel:  base,
		supervisor: sup,
		logManager: logMgr,
	}
}

// Init initializes the model
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		subscribeToLogs(m.logManager),
		refreshProcesses(),
		tickCmd(constants.TUILocalTickInterval),
	)
}

// LogEntryMsg is sent when a new log entry arrives
type LogEntryMsg domain.LogEntry

// ProxyRequestMsg is sent when a new proxy request is recorded
type ProxyRequestMsg proxy.RequestRecord

// ProcessesMsg is sent when processes should be refreshed
type ProcessesMsg []domain.ProcessInfo

// TickMsg is sent periodically
type TickMsg time.Time

// RestartResultMsg is sent when a restart operation completes
type RestartResultMsg struct {
	Process string
	Err     error
}

// RestartResultClearMsg is sent to clear the restart result after a delay
type RestartResultClearMsg struct{}

// RequestDetailMsg is sent when request details are loaded. Seq is the
// fetchRequestDetail call's sequence number (ClientModel.detailFetchSeq at
// dispatch time) — the handler drops any msg whose Seq doesn't match the
// current value, so a stale or superseded fetch can never clobber a newer
// one (D16).
type RequestDetailMsg struct {
	ID      string
	Seq     int
	Details *RequestDetailData
}

// RequestDetailErrorMsg is sent when loading request details fails. Seq is
// the fetchRequestDetail call's sequence number (see RequestDetailMsg).
type RequestDetailErrorMsg struct {
	ID  string
	Seq int
	Err error
}

// RequestDetailData holds the detailed information about a request for TUI display
type RequestDetailData struct {
	ID         string
	Timestamp  string
	Method     string
	URL        string
	Subdomain  string
	StatusCode int
	DurationMs int64
	RemoteAddr string
	// InFlight marks a request whose response is still streaming: Duration
	// renders as "(in flight)" and a nil Details gets an explanatory note
	// instead of being treated as "capture not enabled".
	InFlight bool
	// Stale marks an in-flight request that has been in-flight longer than
	// constants.InFlightStaleAfter (D8, #53): the completion event may have
	// been lost and the true outcome is unknown. Always false when InFlight
	// is false.
	Stale           bool
	RequestHeaders  map[string][]string
	ResponseHeaders map[string][]string
	RequestBody     *BodyData
	ResponseBody    *BodyData
}

// BodyData holds captured body information
type BodyData struct {
	Size            int64
	Truncated       bool
	ContentType     string
	ContentEncoding string
	IsBinary        bool
	Data            string
	// DataBase64 marks Data as base64-encoded (the attach-mode wire format:
	// the API base64-encodes binary bodies). Local mode stores raw bytes and
	// leaves this false, so the hex preview never has to guess — raw bytes
	// that merely LOOK like base64 must not be decoded.
	DataBase64 bool
	// Unavailable is true when the body existed but could no longer be loaded
	// (e.g. its disk file was evicted); UnavailableReason explains why.
	Unavailable       bool
	UnavailableReason string
}

// restartResultClearDelay is how long to show restart result before clearing
const restartResultClearDelay = 3 * time.Second

// restartResultClearCmd returns a command that clears the restart result after a delay
func restartResultClearCmd() tea.Cmd {
	return tea.Tick(restartResultClearDelay, func(t time.Time) tea.Msg {
		return RestartResultClearMsg{}
	})
}

// subscribeToLogs starts log subscription (returns subscription ID for tracking)
// Note: Actual log forwarding is handled by forwardLogs in app.go
func subscribeToLogs(logMgr *logs.Manager) tea.Cmd {
	return func() tea.Msg {
		id, _, err := logMgr.Subscribe(domain.LogFilter{})
		if err != nil {
			return nil
		}
		return subIDMsg(id)
	}
}

type subIDMsg string

// refreshProcesses returns a command to refresh process list
func refreshProcesses() tea.Cmd {
	return func() tea.Msg {
		return ProcessesMsg{}
	}
}

// tickCmd returns a command that ticks after d, driving the periodic
// process-list refresh. LOCAL MODE ONLY since C12: attach mode's tick was
// deleted when the processes stream took over, so the only remaining caller
// reads the in-process supervisor, which costs nothing. The interval stays a
// parameter rather than being inlined so the cadence remains one named
// constant away from the call.
func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}
