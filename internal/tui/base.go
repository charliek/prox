package tui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
	"github.com/charliek/prox/internal/stream"
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

// logRowSpan is the inclusive display-row range an entry occupies in the logs
// viewport after soft-wrap (plan 021 WS4 / Codex #2). When wrap is off,
// First == Last (identity mapping).
type logRowSpan struct {
	First, Last int
}

// maxLogEntries is the maximum number of log entries to keep in memory
const maxLogEntries = 1000

// maxRequestHistory is the maximum number of proxy requests the TUI keeps in
// memory. DEFINED as the server's retention (constants.MaxProxyRequests), not
// as the sync fetch size: scroll-back (D11) pages the ring in
// TUIRequestsSyncLimit-sized steps, so a user who keeps paging can legitimately
// end up holding the WHOLE server ring — a display cap tied to one page's size
// would throw away history the moment the second page landed. Cap ==
// retention means paging can never be starved by the display ring, and the
// trim (keeping newest) only ever discards what the server has itself evicted.
const maxRequestHistory = constants.MaxProxyRequests

// maxErrorDisplayLen is the maximum length of error messages in the status bar
const maxErrorDisplayLen = 60

// HelpConfig configures the help view for different modes
type HelpConfig struct {
	// TitleSuffix is appended to "Prox - Process Manager" (e.g., "(Client Mode)")
	TitleSuffix string
	// QuitMessage describes what happens on quit (e.g., "Quit" or "Quit (daemon continues running)")
	QuitMessage string
}

// BaseModel contains shared fields for both Model and ClientModel
type BaseModel struct {
	// State
	processes     []domain.ProcessInfo
	logEntries    []domain.LogEntry
	proxyRequests []proxy.RequestRecord

	// UI components
	viewport  viewport.Model
	textInput textinput.Model

	// Mode
	mode     Mode
	viewMode ViewMode // Logs or Requests view

	// Filtering
	filterProcesses map[string]bool // Which processes to show
	soloProcess     string          // Single process to show (1-9 keys)
	searchPattern   string          // Current search/filter pattern (the `s` filter)

	// requestSearchQuery is the requests-view `/` search term. It is DELIBERATELY
	// separate from searchPattern: in the requests view `/` navigates (jumps the
	// cursor to matches) rather than filtering, so it composes with — never
	// overwrites — an active `s` filter. Match state is never stored; n/N rescan
	// the filtered list at keypress time (D12/D13).
	requestSearchQuery string

	// logSearchQuery is the logs-view `/` search term. Like requestSearchQuery
	// (and unlike searchPattern, the `s` filter) it NAVIGATES rather than
	// filters: `/` jumps the logs cursor to the first matching line and n/N
	// cycle, leaving every line visible. Match state is never stored; the seek
	// helpers rescan filteredEntries() at keypress time (D6-D8).
	logSearchQuery string

	// logSeq is a session-local monotonic counter stamped onto each ingested
	// LogEntry.DisplaySeq. The logs cursor is anchored by DisplaySeq, not index,
	// so it rides the 1000-entry front-eviction ring without drifting (a bare
	// index would shift when old entries are dropped — see LogEntry.DisplaySeq).
	// This is unrelated to LogEntry.Seq, the server ingest sequence (D7).
	logSeq int64

	// Logs-view search cursor (DisplaySeq-anchored). logCursorSeq names the line
	// the cursor sits on; logCursorIdx is its index in the filtered list. Both are
	// mutated ONLY through setLogCursor so they can never disagree. logCursorSeq
	// 0 is the explicit no-cursor sentinel (stamped DisplaySeqs are always >= 1), and
	// logCursorIdx -1 pairs with it. Unlike the requests cursor, this one exists
	// only while a `/`-search is active and is not scroll-coupled: j/k keep
	// scrolling the viewport, and resolveLogCursor only re-derives the marker
	// index — the scroll-to-match is a one-shot from the /,n,N handlers (D8/D9).
	logCursorSeq int64
	logCursorIdx int

	// Auto-scroll
	followMode bool // Auto-scroll to bottom on new logs

	// Requests-view cursor (ID-anchored). cursorID names the row the cursor
	// sits on; cursorIdx is its index in the filtered list. Both are mutated
	// ONLY through setRequestCursor so they can never disagree. cursorID "" is
	// the explicit no-cursor sentinel (production record IDs are always
	// non-empty — both proxy paths mint them), and cursorIdx -1 pairs with it.
	cursorID  string
	cursorIdx int

	// Last restart result for feedback
	lastRestartProcess string
	lastRestartError   error

	// statusFlash is a short-lived status-bar message (theme cycle, save errors).
	statusFlash string

	// settings are persisted at ~/.prox/tui/config.toml (WS2). View bools drive
	// chrome (ProcessPanel/MenuBar) and log rendering (Timestamps/Wrap) — C4.
	settings Settings

	// projectName is shown in the menu bar (WS3). Set from ClientOptions or
	// resolved to the cwd base in RunClient.
	projectName string

	// Menu bar open state (WS3). openMenu is -1 when closed, otherwise a MenuID.
	// menuHighlight is the full-list index of the highlighted dropdown row.
	// Hit-rects are recorded per render and cleared on close (strix stale-rect
	// discipline / Codex #1).
	openMenu      int
	menuHighlight int
	menuCellHits  []menuCellHit
	menuDropdown  *menuDropdownHit

	// logRowSpans maps DisplaySeq → display-row span in the logs viewport
	// content. Rebuilt every updateViewport (plan 021 WS4 / Codex #2): when wrap
	// is off the span is identity (entry i → {i,i} in filtered order); when wrap
	// is on a long line spans multiple rows. Search origin and cursor visibility
	// translate through these spans so 1-entry==1-row is no longer assumed.
	logRowSpans map[int64]logRowSpan

	// Request detail view
	selectedRequestID string
	requestDetail     *RequestDetailData
	detailLoading     bool
	detailError       error
	// detailRefreshFailed marks a live-refresh attempt (attach mode only —
	// D16) that failed while a snapshot was already on screen: the snapshot
	// is kept rather than replaced by the error view, and
	// formatRequestDetail swaps the "(request in flight...)" note for a
	// refresh-failed one instead. Cleared on any successful detail apply and
	// on leaving the detail view (esc).
	detailRefreshFailed bool

	// Requests scroll-back pagination (D11). The state machine lives in
	// requests_paging.go; the fields live here because handleRequestsSync — a
	// BaseModel method — installs them on every completed sync.
	//
	// pagingCursor is the before_id for the next older page; it is meaningful
	// only while pagingPhase is pagingReady or pagingLoading. pagingGen is
	// bumped by every completed sync, which is what makes an in-flight page
	// result from a superseded generation droppable. pagingErr holds the last
	// transient page failure for the status segment and is cleared by the next
	// trigger or sync.
	pagingCursor string
	pagingPhase  pagingPhase
	pagingGen    int
	pagingErr    error

	// Per-stream health. streamHealth holds the last status reported for each
	// stream; a stream with no entry has reported nothing and renders nothing.
	// streamDropped latches a stream that has been seen reconnecting, so the
	// Syncing that follows a drop keeps rendering as "reconnecting…" while a
	// first-connect Syncing stays silent (see handleStreamStatus).
	streamHealth  map[StreamID]stream.Status
	streamDropped map[StreamID]bool

	// Dimensions
	width  int
	height int
	ready  bool

	// Help configuration
	helpConfig HelpConfig
}

// newBaseModel creates a new BaseModel with the given help configuration
func newBaseModel(helpConfig HelpConfig) BaseModel {
	ti := textinput.New()
	ti.Placeholder = "Type to filter..."
	ti.CharLimit = 100
	ti.Width = 40
	applyTextInputTheme(&ti)

	return BaseModel{
		processes:       make([]domain.ProcessInfo, 0),
		logEntries:      make([]domain.LogEntry, 0),
		proxyRequests:   make([]proxy.RequestRecord, 0),
		textInput:       ti,
		mode:            ModeNormal,
		viewMode:        ViewModeLogs,
		filterProcesses: make(map[string]bool),
		streamHealth:    make(map[StreamID]stream.Status),
		streamDropped:   make(map[StreamID]bool),
		followMode:      true,
		logCursorIdx:    -1, // no-cursor sentinel (pairs with logCursorSeq 0)
		helpConfig:      helpConfig,
		settings:        DefaultSettings(),
		openMenu:        -1, // closed
	}
}

// applyTextInputTheme sets prompt/cursor/text styles from the active theme.
func applyTextInputTheme(ti *textinput.Model) {
	th := CurrentTheme()
	if th == nil {
		return
	}
	ti.PromptStyle = lipgloss.NewStyle().Foreground(th.FooterKey)
	ti.TextStyle = lipgloss.NewStyle().Foreground(th.FG)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(th.Cursor)
}

// handleWindowSize handles window resize messages.
func (b *BaseModel) handleWindowSize(msg tea.WindowSizeMsg) {
	b.width = msg.Width
	b.height = msg.Height
	b.relayout()
}

// chromeAbove is the number of rows above the viewport (menu bar + process
// panel). Derived from ACTUAL enabled chrome — not fixed reservations (Codex #8).
func (b *BaseModel) chromeAbove() int {
	h := 0
	if b.settings.MenuBar {
		h++ // menu bar row
	}
	if b.settings.ProcessPanel {
		h += 2 // content line + Header MarginBottom
	}
	return h
}

// chromeBelow is status bar + key-hint footer.
func (b *BaseModel) chromeBelow() int {
	return 2
}

// chromeHeight is the total non-viewport chrome.
func (b *BaseModel) chromeHeight() int {
	return b.chromeAbove() + b.chromeBelow()
}

// defaultChromeHeight is chrome under DefaultSettings (menu+panel on).
// Test helpers that size a WindowSizeMsg for a target viewport height use this.
func defaultChromeHeight() int {
	s := DefaultSettings()
	h := 2 // status + hint
	if s.MenuBar {
		h++
	}
	if s.ProcessPanel {
		h += 2
	}
	return h
}

// relayout derives viewport geometry from enabled chrome rows. Called on
// WindowSizeMsg AND every visibility toggle (a toggle emits no resize, so
// without this the viewport keeps stale geometry — Codex #8). C4 flips
// ProcessPanel and calls relayout the same way.
func (b *BaseModel) relayout() {
	if b.width <= 0 || b.height <= 0 {
		return
	}
	vpH := b.height - b.chromeHeight()
	if vpH < 1 {
		vpH = 1
	}
	headerH := b.chromeAbove()
	if !b.ready {
		b.viewport = viewport.New(b.width, vpH)
		b.viewport.YPosition = headerH
		b.ready = true
	} else {
		b.viewport.Width = b.width
		b.viewport.Height = vpH
		b.viewport.YPosition = headerH
	}
}

// handleLogEntry handles a new log entry message: one append followed by one
// render.
func (b *BaseModel) handleLogEntry(entry domain.LogEntry) {
	// Check if we're at/near bottom BEFORE adding new content
	wasNearBottom := b.isNearBottom()
	b.appendLogEntry(entry)
	b.renderAfterLogEntries(wasNearBottom)
}

// appendLogEntry stamps one entry and appends it to the ring WITHOUT
// rendering. Split out of handleLogEntry so a sync batch (C9) can apply a
// thousand entries and render once; every arrival path — live entry, batch
// entry, synthetic notice — goes through here, so the DisplaySeq stamping and
// the eviction trim are identical for all of them.
func (b *BaseModel) appendLogEntry(entry domain.LogEntry) {
	// Stamp a session-local monotonic DisplaySeq so the logs search cursor can
	// anchor to this line's identity across the front-eviction ring below. This
	// overwrites nothing on the wire: the server's LogEntry.Seq is a separate
	// field and is left untouched (D7).
	b.logSeq++
	entry.DisplaySeq = b.logSeq
	b.logEntries = append(b.logEntries, entry)
	// Keep only last entries - create new slice to release memory from old entries
	if len(b.logEntries) > maxLogEntries {
		newEntries := make([]domain.LogEntry, maxLogEntries)
		copy(newEntries, b.logEntries[len(b.logEntries)-maxLogEntries:])
		b.logEntries = newEntries
	}
}

// renderAfterLogEntries is the shared render tail for log arrivals — one live
// entry or a whole sync batch. wasNearBottom is the viewport's position
// sampled BEFORE the entries were appended (its meaning is lost afterwards).
// Batches deliberately share it so a 1000-entry replay costs exactly one
// render, mirroring renderAfterProxyRequests.
func (b *BaseModel) renderAfterLogEntries(wasNearBottom bool) {
	b.updateViewport()

	// If user was at bottom, re-enable follow mode and stay at bottom — UNLESS an
	// active logs search has parked the cursor off the newest row. landLogSearchJump
	// deliberately disengages follow on an off-newest match; with a short/full
	// viewport that match is still "near bottom", so a streaming arrival would
	// otherwise silently flip follow back on and let resolveLogCursor drag the ❯
	// marker off the match (search position lost mid-stream). While a search is
	// parked (query set, follow off), leave follow disengaged.
	searchParked := b.logSearchQuery != "" && !b.followMode
	if wasNearBottom && !searchParked {
		b.followMode = true
		b.viewport.GotoBottom()
	} else if b.followMode {
		b.viewport.GotoBottom()
	}
}

// handleLogsSync applies one completed logs-stream synchronization (C9): the
// optional notice line first, then the batch entries oldest-first, then ONE
// render.
//
// The handler is deliberately dumb — append, don't merge. All the hard parts
// (which entries to fetch, epoch changes, dropping entries this model has
// already been shown) live in the sync layer, which excludes overlap by
// cursor BEFORE delivery (logs_sync.go). Log lines have no identity to merge
// on the way proxy requests do (the same line can legitimately repeat), so a
// model-side dedupe is not available even in principle.
//
// Entries that predate an epoch change stay in the list as history: they are
// what the previous daemon run printed, and the notice line marks the seam.
func (b *BaseModel) handleLogsSync(msg LogsSyncMsg) {
	if msg.Notice == "" && len(msg.Entries) == 0 {
		return // nothing changed: a caught-up reconnect must not force a render
	}

	wasNearBottom := b.isNearBottom()
	if msg.Notice != "" {
		b.appendLogEntry(systemLogEntry(msg.Notice))
	}
	for _, entry := range msg.Entries {
		b.appendLogEntry(entry)
	}
	b.renderAfterLogEntries(wasNearBottom)
}

// handleProxyRequest handles a new proxy request message: one monotonic merge
// followed by one render.
func (b *BaseModel) handleProxyRequest(req proxy.RequestRecord) {
	b.mergeProxyRequest(req)
	b.renderAfterProxyRequests()
}

// mergeProxyRequest applies one record to the list as a monotonic two-state
// transition keyed by ID, mirroring proxy.Upsert's table:
//
//	absent                            → append (with the eviction trim)
//	existing in-flight, incoming any  → replace in place
//	existing final                    → no-op (final is terminal)
//
// The one deliberate difference from proxy.Upsert is the in-flight → in-flight
// row: the ring no-ops it as a duplicate delivery, while the list takes the
// newer copy. Both are correct (the two copies differ in nothing that matters),
// and taking it keeps the display tracking the freshest server view.
//
// The terminal-final row is what makes replaying a snapshot alongside live
// events safe in any interleaving (C6): a snapshot's stale in-flight copy can
// never regress a completion that already arrived live, and a duplicate final
// can never re-render one. In-place replacement keeps every other row's index
// stable, and the ID-anchored cursor (resolveRequestCursor) rides along on its
// row regardless.
//
// Records with no ID (never produced by either proxy path) always append: they
// have no identity to merge on.
func (b *BaseModel) mergeProxyRequest(req proxy.RequestRecord) {
	if b.upsertExistingRequest(req) {
		return
	}

	b.proxyRequests = append(b.proxyRequests, req)
	b.trimRequestHistory()
}

// upsertExistingRequest applies the monotonic rule to an ALREADY-PRESENT row and
// reports whether it found one. Split out of mergeProxyRequest so the
// scroll-back page apply (prependOlderRequests) resolves overlaps by exactly the
// same rule while placing its NOVEL records at the front rather than the end.
// The scan runs newest-first, since that's where a live in-flight row lives.
func (b *BaseModel) upsertExistingRequest(req proxy.RequestRecord) bool {
	if req.ID == "" {
		return false // no identity to merge on
	}
	for i := len(b.proxyRequests) - 1; i >= 0; i-- {
		if b.proxyRequests[i].ID != req.ID {
			continue
		}
		b.applyMonotonicAt(i, req)
		return true
	}
	return false
}

// applyMonotonicAt is the two-state transition itself, at a known index.
func (b *BaseModel) applyMonotonicAt(i int, req proxy.RequestRecord) {
	b.proxyRequests[i] = monotonicWinner(b.proxyRequests[i], req)
}

// monotonicWinner is the rule two copies of the same request resolve by: an
// in-flight incumbent yields to the incoming copy, a final one is terminal. Free
// function so it also serves records not yet in the list (a scroll-back page's
// own block — see spliceOlderRequests).
func monotonicWinner(existing, incoming proxy.RequestRecord) proxy.RequestRecord {
	if existing.InFlight {
		return incoming
	}
	return existing
}

// trimRequestHistory enforces the display cap, KEEPING THE NEWEST records — the
// eviction semantics of every growth path (live arrival, sync batch,
// scroll-back page).
func (b *BaseModel) trimRequestHistory() {
	b.keepNewestRequests(maxRequestHistory)
}

// keepNewestRequests is the list's single front-eviction primitive: it shrinks
// proxyRequests to its newest n records. Both callers that drop from the front
// go through it (the cap trim above and D12's drop-on-resync).
//
// The retained records slide down inside the EXISTING array and the vacated
// tail is cleared, which releases the dropped records (their URL strings and
// detail pointers) without releasing the array itself. Keeping the array is
// what makes eviction at the cap cheap: proxyRequests holds up to
// maxRequestHistory == constants.MaxProxyRequests records, so reallocating the
// whole list on every arrival past the cap — and again on the append that
// follows, since a right-sized copy leaves no spare capacity — would turn a
// steady request stream into megabytes of garbage per request, and a sync batch
// into a quadratic one.
func (b *BaseModel) keepNewestRequests(n int) {
	dropped := len(b.proxyRequests) - n
	if dropped <= 0 {
		return
	}
	copy(b.proxyRequests, b.proxyRequests[dropped:])
	clear(b.proxyRequests[n:])
	b.proxyRequests = b.proxyRequests[:n]
}

// renderAfterProxyRequests is the shared render tail for request arrivals —
// one live record or a whole sync batch. Batches deliberately share it so a
// 1000-record replay costs exactly one render.
func (b *BaseModel) renderAfterProxyRequests() {
	// In the detail view an arrival updates only the list data: the viewport is
	// showing the open detail, so re-rendering/scrolling it would yank the
	// reader (today's GotoBottom wart). No follow change either. C4 layers the
	// in-place detail refresh on top of this guard.
	if b.viewMode == ViewModeRequestDetail {
		return
	}

	// In the requests view, follow re-engagement is cursor-based, never
	// isNearBottom-based: with a short list that fits the viewport AtBottom()
	// is ALWAYS true, so consulting it would re-engage follow on every arrival
	// and yank the cursor off the row the user navigated to. Arrivals never
	// change followMode here; updateViewport re-resolves the cursor and, per
	// D7, either GotoBottom (follow on) or keeps the cursor row visible.
	if b.viewMode == ViewModeRequests {
		b.updateViewport()
		return
	}

	// Logs view: the requests list isn't on screen, so keep today's
	// isNearBottom follow semantics for the (logs) viewport untouched.
	wasNearBottom := b.isNearBottom()
	b.updateViewport()
	if wasNearBottom {
		b.followMode = true
		b.viewport.GotoBottom()
	} else if b.followMode {
		b.viewport.GotoBottom()
	}
}

// handleRequestsSync applies one completed stream synchronization (C6) by
// REBUILDING the list from the sync payload: the REST snapshot oldest-first,
// then the events buffered during the fetch in arrival order, through the
// monotonic merge — so any interleaving of {snapshot copy, live copy,
// completion} converges on the final record with no duplicate rows and no
// regressions. Nothing from before the sync survives it.
//
// Rebuild — not merge-into-the-old-list-and-cut — is D12's drop-on-resync.
// Requests carry no sequence numbers (unlike logs), so a client cannot prove
// contiguity across a reconnect gap; the only list whose ordering is
// KNOWN-correct is the sync payload itself, which the server produced in one
// coherent pass. Cutting the merged list at the snapshot's oldest record was
// tried first and is unsound three ways (cursor review, C7): an empty
// snapshot wiped the buffered live events merged just before it; a
// snapshot-oldest that was NOVEL to the list appended at the back, so the cut
// deleted every older-but-retained row in front of it; and a cap trim could
// evict the anchor entirely, turning the cut into a silent no-op that left a
// hole for the next page to prepend across.
//
// The rebuild also subsumes the prior-epoch stale sweep this used to run: a
// pre-sync row the server no longer has is now DROPPED rather than kept and
// marked stale — the server we just synchronized against has no record of it,
// so showing it at all would be showing a dead epoch. requestIsStale keeps
// only the age-based rule (D8, #53).
//
// The cost is deliberate: a reconnect discards paged-in history and the user
// re-pages it (the server retains constants.MaxProxyRequests; reconnects are
// rare). An EMPTY snapshot plus no buffered events yields an empty list — a
// fresh or replaced daemon, and a cleared view is the truth.
//
// Everything renders ONCE at the end: a full 1000-record ring replay must not
// re-render a thousand times. The cursor is ID-anchored, so a user parked on a
// row keeps it when the row survives the rebuild and falls back by index when
// it does not (resolveRequestCursor); follow mode re-pins to the newest row.
// The pagination install is unconditional and last (D11): the cursor and
// generation must describe THIS payload's window.
func (b *BaseModel) handleRequestsSync(msg RequestsSyncMsg) {
	b.proxyRequests = nil
	for _, req := range msg.Snapshot {
		b.mergeProxyRequest(req)
	}
	for _, req := range msg.Buffered {
		b.mergeProxyRequest(req)
	}

	b.installRequestsPaging(msg.NextBeforeID)
	b.renderAfterProxyRequests()
}

// requestIsStale reports whether a row renders as stale (D8, #53): it has been
// in-flight past constants.InFlightStaleAfter. Final records are never stale.
// (Prior-epoch in-flight rows — ones the server lost across a reconnect — are
// dropped by handleRequestsSync's rebuild rather than marked; the age rule is
// the one staleness signal left.)
func (b *BaseModel) requestIsStale(req proxy.RequestRecord) bool {
	if !req.InFlight {
		return false
	}
	return req.StaleAt(time.Now())
}

// handleFilterKey handles keys in filter mode
func (b *BaseModel) handleFilterKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		return true, nil

	case "enter":
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateViewport()
		return true, nil

	case "a":
		// Select all
		for name := range b.filterProcesses {
			b.filterProcesses[name] = true
		}
		return true, nil

	case "n":
		// Select none
		for name := range b.filterProcesses {
			b.filterProcesses[name] = false
		}
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	return true, cmd
}

// handleSearchKey handles keys in search mode
func (b *BaseModel) handleSearchKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		return true, nil

	case "enter":
		b.mode = ModeNormal
		b.textInput.Blur()
		if b.viewMode == ViewModeRequests {
			// Requests view: `/` is navigation, not filtration. Commit the query
			// to requestSearchQuery (searchPattern — the `s` filter — untouched)
			// and jump the cursor to the first match at-or-after it (D12).
			b.requestSearchQuery = b.textInput.Value()
			b.jumpToRequestSearchMatch()
			b.updateViewport()
			return true, nil
		}
		// Logs view: `/` is navigation, not filtration (D6/D8) — it mirrors the
		// requests view. Commit the query to logSearchQuery (searchPattern — the
		// `s` filter — is untouched) and jump the cursor to the first match
		// at-or-after the current position, wrapping. The scroll-to-match is a
		// one-shot here rather than wired into updateViewport, which also runs on
		// streaming arrivals and free j/k scroll where re-scrolling would fight
		// the reader.
		b.logSearchQuery = b.textInput.Value()
		b.seekLogSearchMatch(0)
		b.updateViewport()
		b.ensureLogCursorVisible()
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	return true, cmd
}

// handleStringFilterKey handles keys in string filter mode
func (b *BaseModel) handleStringFilterKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		b.searchPattern = ""
		b.updateViewport()
		return true, nil

	case "enter":
		b.searchPattern = b.textInput.Value()
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateViewport()
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	// Live update filter
	b.searchPattern = b.textInput.Value()
	b.updateViewport()
	return true, cmd
}

// handleHelpKey handles keys in help mode
func (b *BaseModel) handleHelpKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "?", "q", "enter":
		b.mode = ModeNormal
		return true
	}
	return true
}

// cycleTheme advances the active theme, persists the choice, and schedules a
// status-bar flash. Mid-session callers must re-render the viewport (cached
// styled strings) and re-apply textinput styles (WS1).
func (b *BaseModel) cycleTheme() tea.Cmd {
	name, theme := CycleTheme(CurrentThemeName())
	currentThemeName = name
	SetTheme(theme)
	b.settings.Theme = name

	if err := SaveSettings(b.settings); err != nil {
		b.statusFlash = "settings not saved: " + err.Error()
	} else {
		b.statusFlash = "theme: " + name
	}
	applyTextInputTheme(&b.textInput)
	b.updateViewport()
	return statusFlashClearCmd()
}

// setViewMode switches the active view and tears down detail state when leaving
// detail (Codex #4 — that cleanup lived only in the Esc branch before). Tab,
// menu radios, and Esc all route through here.
func (b *BaseModel) setViewMode(mode ViewMode) {
	if b.viewMode == mode {
		return
	}
	if b.viewMode == ViewModeRequestDetail && mode != ViewModeRequestDetail {
		b.selectedRequestID = ""
		b.requestDetail = nil
		b.detailError = nil
		b.detailRefreshFailed = false
		b.detailLoading = false
	}
	b.viewMode = mode
	b.updateViewport()
}

// toggleFollow is the shared Follow toggle used by the F key and the View menu
// check (WS3 — no behavior duplication).
func (b *BaseModel) toggleFollow() {
	if b.viewMode == ViewModeRequests {
		b.followMode = !b.followMode
		if b.followMode {
			requests := b.filteredProxyRequests()
			b.setRequestCursor(requests, len(requests)-1)
		}
		b.updateViewport()
		return
	}
	b.followMode = !b.followMode
	if b.followMode {
		b.viewport.GotoBottom()
	}
}

// toggleProcessPanel flips settings.ProcessPanel, persists, and relayouts
// (viewport height changes by ±2 — plan 021 WS4 / Codex #8).
func (b *BaseModel) toggleProcessPanel() tea.Cmd {
	b.settings.ProcessPanel = !b.settings.ProcessPanel
	b.relayout()
	if err := SaveSettings(b.settings); err != nil {
		b.statusFlash = "settings not saved: " + err.Error()
		return statusFlashClearCmd()
	}
	return nil
}

// toggleTimestamps flips settings.Timestamps, persists, and re-renders (log
// lines are cached styled strings — plan 021 WS4).
func (b *BaseModel) toggleTimestamps() tea.Cmd {
	b.settings.Timestamps = !b.settings.Timestamps
	var cmd tea.Cmd
	if err := SaveSettings(b.settings); err != nil {
		b.statusFlash = "settings not saved: " + err.Error()
		cmd = statusFlashClearCmd()
	}
	b.updateViewport()
	return cmd
}

// toggleWrap flips settings.Wrap, persists, and re-renders with DisplaySeq
// top-anchor preservation when not following (plan 021 WS4 / Codex #2).
func (b *BaseModel) toggleWrap() tea.Cmd {
	var anchorSeq int64
	if !b.followMode && b.viewMode == ViewModeLogs {
		anchorSeq = b.displaySeqAtYOffset(b.viewport.YOffset)
	}
	b.settings.Wrap = !b.settings.Wrap
	var cmd tea.Cmd
	if err := SaveSettings(b.settings); err != nil {
		b.statusFlash = "settings not saved: " + err.Error()
		cmd = statusFlashClearCmd()
	}
	b.updateViewport()
	if b.viewMode == ViewModeLogs {
		if b.followMode {
			b.viewport.GotoBottom()
		} else if anchorSeq != 0 {
			if sp, ok := b.logRowSpans[anchorSeq]; ok {
				b.viewport.SetYOffset(sp.First)
			}
		}
	}
	return cmd
}

// displaySeqAtYOffset returns the DisplaySeq of the entry whose span contains
// display row y, or 0 when unknown (no spans / empty). Used as the wrap-toggle
// top anchor (Codex #2).
func (b *BaseModel) displaySeqAtYOffset(y int) int64 {
	for seq, sp := range b.logRowSpans {
		if y >= sp.First && y <= sp.Last {
			return seq
		}
	}
	return 0
}

// handleNavigationKey handles common navigation keys.
// Returns whether the key was handled and an optional command.
func (b *BaseModel) handleNavigationKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "t":
		return true, b.cycleTheme()

	case "m":
		// Toggle menu bar visibility and persist (WS3).
		return true, b.toggleMenuBar()

	case "p":
		// Process panel visibility (WS4). Textinput modes never reach here
		// (Codex #4 routing); verified free in discovery.
		return true, b.toggleProcessPanel()

	case "T":
		// Timestamps in log lines (WS4). Distinct from `t` (theme cycle).
		return true, b.toggleTimestamps()

	case "w":
		// Soft-wrap log lines (WS4 / Codex #2).
		return true, b.toggleWrap()

	case "v":
		// Open the View menu when the bar is visible. `v` was free; Theme has
		// no mnemonic (`t` stays cycle — panel B1). Keyboard path to Theme:
		// open View then Right/Tab sibling-switch.
		//
		// NOTE: `f` is NOT a Filter-menu mnemonic yet — it still opens
		// ModeFilter (C8 rebinds `f` to the Filter dropdown). Until then the
		// Filter cell is click/sibling-only (pinned C3 resolution).
		if b.settings.MenuBar {
			b.openMenuFirst(MenuView)
		}
		return true, nil

	case "tab":
		// Toggle between Logs and Requests views (only if not in detail view)
		switch b.viewMode {
		case ViewModeLogs:
			b.setViewMode(ViewModeRequests)
		case ViewModeRequests:
			b.setViewMode(ViewModeLogs)
		}
		// In detail view, tab does nothing
		return true, nil

	case "?":
		b.mode = ModeHelp
		return true, nil

	case "f":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeFilter
			b.textInput.Focus()
		}
		return true, nil

	case "/":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeSearch
			b.textInput.SetValue("")
			b.textInput.Focus()
		}
		return true, nil

	case "s":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeStringFilter
			b.textInput.SetValue("")
			b.textInput.Focus()
		}
		return true, nil

	case "n", "N":
		// Search navigation: n jumps to the next match strictly after the
		// cursor, N to the previous, both wrapping. Handled in the requests view
		// (D13) and the logs view (D8); a no-op query in either view leaves the
		// cursor put. Unhandled in the detail view.
		dir := 1
		if msg.String() == "N" {
			dir = -1
		}
		switch b.viewMode {
		case ViewModeRequests:
			b.cycleRequestSearchMatch(dir)
			b.updateViewport()
			return true, nil
		case ViewModeLogs:
			// One-shot scroll to the landed match, after the re-render (updateViewport
			// deliberately never scrolls the logs viewport — see seekLogSearchMatch).
			b.seekLogSearchMatch(dir)
			b.updateViewport()
			b.ensureLogCursorVisible()
			return true, nil
		}
		return false, nil

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Solo process in logs view only (1-9 keys do nothing in requests view)
		if b.viewMode == ViewModeLogs {
			idx := int(msg.String()[0] - '1')
			if idx < len(b.processes) {
				name := b.processes[idx].Name
				if b.soloProcess == name {
					// Toggle off
					b.soloProcess = ""
				} else {
					b.soloProcess = name
				}
				b.updateViewport()
			}
		}
		return true, nil

	case "esc":
		// In detail view, go back to requests list (via setViewMode so detail
		// teardown lives in one place — Codex #4).
		if b.viewMode == ViewModeRequestDetail {
			b.setViewMode(ViewModeRequests)
			return true, nil
		}
		// Clear filters and both views' search queries (D13/D8). Resetting the
		// logs cursor to the no-cursor sentinel makes the next `/` seed its
		// origin from the viewport again rather than the stale prior match.
		b.soloProcess = ""
		b.searchPattern = ""
		b.requestSearchQuery = ""
		b.logSearchQuery = ""
		b.logCursorSeq = 0
		b.logCursorIdx = -1
		b.updateViewport()
		return true, nil

	case "up", "k":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(-1)
			return true, nil
		}
		b.viewport.LineUp(1)
		b.followMode = false
		return true, nil

	case "down", "j":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(1)
			return true, nil
		}
		b.viewport.LineDown(1)
		return true, nil

	case "pgup":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(-b.halfPageStep())
			return true, nil
		}
		b.viewport.HalfViewUp()
		b.followMode = false
		return true, nil

	case "pgdown":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(b.halfPageStep())
			return true, nil
		}
		b.viewport.HalfViewDown()
		return true, nil

	case "home", "g":
		if b.viewMode == ViewModeRequests {
			requests := b.filteredProxyRequests()
			b.setRequestCursor(requests, 0)
			b.followMode = false
			b.updateViewport()
			return true, nil
		}
		b.viewport.GotoTop()
		b.followMode = false
		return true, nil

	case "end", "G":
		if b.viewMode == ViewModeRequests {
			requests := b.filteredProxyRequests()
			b.followMode = true
			b.setRequestCursor(requests, len(requests)-1)
			b.updateViewport()
			return true, nil
		}
		b.viewport.GotoBottom()
		b.followMode = true
		return true, nil

	case "F":
		b.toggleFollow()
		return true, nil
	}

	return false, nil
}

// moveRequestCursor moves the requests-view cursor by delta rows (negative =
// up, positive = down) through the sole mutator, then re-renders. The follow
// rule is sign-driven and matches the per-key semantics exactly: upward
// movement (k/pgup) disengages follow, while downward movement (j/pgdown)
// re-engages follow only when the cursor lands on the last (newest) row — the
// cursor analog of "scrolled back to the bottom".
func (b *BaseModel) moveRequestCursor(delta int) {
	requests := b.filteredProxyRequests()
	b.setRequestCursor(requests, b.cursorIdx+delta)
	if delta < 0 {
		b.followMode = false
	} else if len(requests) > 0 && b.cursorIdx == len(requests)-1 {
		b.followMode = true
	}
	b.updateViewport()
}

// halfPageStep is the cursor step for pgup/pgdown in the requests view: half a
// viewport page, matching the logs view's HalfView semantics, and never less
// than one row so paging always makes progress on a tiny viewport.
func (b *BaseModel) halfPageStep() int {
	step := b.viewport.Height / 2
	if step < 1 {
		step = 1
	}
	return step
}

// requestMatchesSearch reports whether a request matches the requests-view `/`
// query: a case-insensitive substring over URL, Method, and Subdomain — the
// same fields the `s` filter matches (D12).
func requestMatchesSearch(req proxy.RequestRecord, query string) bool {
	return containsIgnoreCase(req.URL, query) ||
		containsIgnoreCase(req.Method, query) ||
		containsIgnoreCase(req.Subdomain, query)
}

// jumpToRequestSearchMatch moves the cursor to the first row matching
// requestSearchQuery at-or-after the current cursor, wrapping (D12). "At-or-after"
// is deliberate: a fresh search may already sit on the best match; n advances.
// No-op when the query is empty or nothing matches (cursor unmoved). A match
// disengages follow so the jump sticks (resolveRequestCursor would otherwise
// re-pin to the newest row); it never RE-engages follow — a search jump is
// positioning, not scrolling intent (D13).
func (b *BaseModel) jumpToRequestSearchMatch() {
	b.seekRequestSearchMatch(0)
}

// cycleRequestSearchMatch moves the cursor to the next (dir +1) or previous
// (dir -1) matching row STRICTLY past the current cursor, wrapping (D13).
func (b *BaseModel) cycleRequestSearchMatch(dir int) {
	b.seekRequestSearchMatch(dir)
}

// seekRequestSearchMatch scans the filtered list for a matching row and moves
// the cursor to it. dir 0 = at-or-after the cursor (the `/`-commit jump,
// includes the cursor's own row); dir +1/-1 = strictly next/previous (n/N).
// Derived at keypress time — no match list is ever stored (D13).
func (b *BaseModel) seekRequestSearchMatch(dir int) {
	if b.requestSearchQuery == "" {
		return
	}
	requests := b.filteredProxyRequests()
	n := len(requests)
	if n == 0 {
		return
	}
	start := b.cursorIdx
	if start < 0 {
		start = 0
	}
	if dir == 0 {
		// At-or-after: offsets 0..n-1 forward, cursor row first.
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			if requestMatchesSearch(requests[idx], b.requestSearchQuery) {
				b.landSearchJump(requests, idx, n)
				return
			}
		}
		return
	}
	// Strictly past the cursor: offsets 1..n-1 in dir, wrapping. The cursor's
	// own row is never revisited, so n/N are no-ops when it is the sole match.
	for i := 1; i < n; i++ {
		idx := ((start+dir*i)%n + n) % n
		if requestMatchesSearch(requests[idx], b.requestSearchQuery) {
			b.landSearchJump(requests, idx, n)
			return
		}
	}
}

// landSearchJump places the cursor on a search match. Follow mode is
// disengaged only when the jump moves the cursor off the newest row — a jump
// must stick against follow's pin-to-newest, but a match landing ON the
// newest row leaves follow untouched (never re-engaged either; search jumps
// are positioning, not scrolling intent).
func (b *BaseModel) landSearchJump(requests []proxy.RequestRecord, idx, n int) {
	if idx != n-1 {
		b.followMode = false
	}
	b.setRequestCursor(requests, idx)
}

// requestSearchMatchInfo derives the status-bar match indicator for the current
// requestSearchQuery over requests (the filtered list): total match count, and
// the cursor's 1-based position among matches (0 when the cursor is not on a
// match). Never stored — recomputed for the status bar (D13). requests is
// passed in rather than recomputed here so statusBar can share a single
// filteredProxyRequests() scan with the visible/total count.
func (b *BaseModel) requestSearchMatchInfo(requests []proxy.RequestRecord) (position, total int) {
	if b.requestSearchQuery == "" {
		return 0, 0
	}
	for i, req := range requests {
		if requestMatchesSearch(req, b.requestSearchQuery) {
			total++
			if i == b.cursorIdx {
				position = total
			}
		}
	}
	return position, total
}

// logMatchesSearch reports whether a log line matches the logs-view `/` query:
// a case-insensitive substring over the line text (D8).
func logMatchesSearch(entry domain.LogEntry, query string) bool {
	return containsIgnoreCase(entry.Line, query)
}

// seekLogSearchMatch scans the filtered log entries for a matching line and
// moves the logs cursor to it. dir 0 = at-or-after the origin (the `/`-commit
// jump, includes the origin row); dir +1/-1 = strictly next/previous (n/N),
// wrapping. Mirrors seekRequestSearchMatch: derived at keypress time, no match
// list is ever stored (D8).
func (b *BaseModel) seekLogSearchMatch(dir int) {
	if b.logSearchQuery == "" {
		return
	}
	entries := b.filteredEntries()
	n := len(entries)
	if n == 0 {
		return
	}
	start := b.logSearchOriginIdx(entries)
	if dir == 0 {
		// At-or-after: offsets 0..n-1 forward, origin row first.
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			if logMatchesSearch(entries[idx], b.logSearchQuery) {
				b.landLogSearchJump(entries, idx, n)
				return
			}
		}
		// No match: clear any cursor a PRIOR search left, so a stale ❯ marker
		// doesn't linger on a non-matching row while the status shows "(0 matches)"
		// (CodeRabbit). setLogCursor(nil, ...) resets to the no-cursor sentinel.
		b.setLogCursor(nil, 0)
		return
	}
	// Strictly past the cursor: offsets 1..n-1 in dir, wrapping. The origin row
	// is never revisited, so n/N are no-ops when it is the sole match.
	for i := 1; i < n; i++ {
		idx := ((start+dir*i)%n + n) % n
		if logMatchesSearch(entries[idx], b.logSearchQuery) {
			b.landLogSearchJump(entries, idx, n)
			return
		}
	}
}

// logSearchOriginIdx returns the index in entries from which a seek should
// begin. With a cursor already anchored (logCursorSeq set), it re-resolves that
// DisplaySeq to its current index — surviving the eviction ring — and clamps the
// last-known index when the anchored line has been evicted. On the FIRST search
// (no cursor yet), it seeds from what the user is looking at: the newest row
// under follow, else the top visible row derived from the viewport offset, so
// `/` searches from the region on screen (D8).
func (b *BaseModel) logSearchOriginIdx(entries []domain.LogEntry) int {
	n := len(entries)
	if b.logCursorSeq != 0 {
		for i, e := range entries {
			if e.DisplaySeq == b.logCursorSeq {
				return i
			}
		}
		// Anchored line evicted: fall back to the clamped last-known index.
		return clampIndex(b.logCursorIdx, n)
	}
	if b.followMode {
		return n - 1
	}
	// YOffset is a display row, not an entry index — translate through spans
	// (Codex #2). When wrap is off spans are identity, so this matches the
	// pre-C4 clampIndex(YOffset, n) behavior.
	return b.entryIndexContainingRow(entries, b.viewport.YOffset)
}

// entryIndexContainingRow returns the filtered-list index of the entry whose
// display-row span contains row. Falls back to clamping row as an entry index
// when spans are missing (should not happen after updateViewport).
func (b *BaseModel) entryIndexContainingRow(entries []domain.LogEntry, row int) int {
	n := len(entries)
	if n == 0 {
		return 0
	}
	for i, e := range entries {
		sp, ok := b.logRowSpans[e.DisplaySeq]
		if !ok {
			continue
		}
		if row >= sp.First && row <= sp.Last {
			return i
		}
	}
	return clampIndex(row, n)
}

// clampIndex clamps idx into [0, n-1] for a non-empty n (callers guard n > 0).
func clampIndex(idx, n int) int {
	if idx < 0 {
		return 0
	}
	if idx > n-1 {
		return n - 1
	}
	return idx
}

// landLogSearchJump places the logs cursor on a search match. Mirrors
// landSearchJump: follow is disengaged only when the jump moves the cursor off
// the newest row — a jump must stick against resolveLogCursor's pin-to-newest
// and handleLogEntry's auto-GotoBottom, but a match landing ON the newest row
// leaves follow untouched (never re-engaged; a search jump is positioning, not
// scrolling intent).
func (b *BaseModel) landLogSearchJump(entries []domain.LogEntry, idx, n int) {
	if idx != n-1 {
		b.followMode = false
	}
	b.setLogCursor(entries, idx)
}

// setLogCursor is the SOLE mutator of the logs-view search cursor. It clamps
// idx into [0, len(entries)-1] and updates logCursorIdx and logCursorSeq
// together so the pair can never disagree; an empty list resets to the
// no-cursor sentinel (logCursorIdx -1, logCursorSeq 0). Mirrors setRequestCursor.
func (b *BaseModel) setLogCursor(entries []domain.LogEntry, idx int) {
	if len(entries) == 0 {
		b.logCursorIdx = -1
		b.logCursorSeq = 0
		return
	}
	idx = clampIndex(idx, len(entries))
	b.logCursorIdx = idx
	b.logCursorSeq = entries[idx].DisplaySeq
}

// resolveLogCursor re-anchors the logs search cursor against the current
// filtered list at render time (called from updateViewport only while a search
// is active), so the row marker stays on the searched-to line as entries stream
// in and the eviction ring shifts indices. Mirrors resolveRequestCursor but
// anchors by DisplaySeq (logs have no ID) and NEVER scrolls — the logs viewport scroll
// stays owned by j/k, follow, and the one-shot ensureLogCursorVisible.
//
// Unlike the requests cursor (which always exists), the logs cursor is
// search-scoped: logCursorSeq 0 means no jump has landed (fresh search with no
// match, or a cleared one), so there is NO marker — we return the sentinel
// rather than manufacturing a row-0 cursor. Precedence when a cursor DOES exist:
//   - follow mode pins the cursor to the newest (last) row;
//   - else if logCursorSeq is still present, its current index;
//   - else the stale logCursorIdx is clamped and re-anchored (line evicted).
func (b *BaseModel) resolveLogCursor(entries []domain.LogEntry) {
	if b.logCursorSeq == 0 {
		b.logCursorIdx = -1 // no active cursor: sentinel, no marker
		return
	}
	if len(entries) == 0 {
		b.setLogCursor(entries, 0) // resets to the no-cursor sentinel
		return
	}
	if b.followMode {
		b.setLogCursor(entries, len(entries)-1)
		return
	}
	for i, e := range entries {
		if e.DisplaySeq == b.logCursorSeq {
			b.setLogCursor(entries, i)
			return
		}
	}
	// Anchored line evicted: clamp the last-known index and re-anchor.
	b.setLogCursor(entries, b.logCursorIdx)
}

// ensureLogCursorVisible scrolls the logs viewport the minimum amount needed to
// bring the cursor entry's display-row span on-screen. Called ONLY from the
// /,n,N handlers as a one-shot (mirrors ensureCursorVisible but is deliberately
// NOT wired into updateViewport, so streaming re-renders and free j/k scroll
// are never yanked). Translates through logRowSpans (Codex #2 / adversarial B3).
func (b *BaseModel) ensureLogCursorVisible() {
	if b.logCursorIdx < 0 {
		return // no-cursor sentinel (empty list or no active search)
	}
	// A grown viewport (or shrunk list) can leave YOffset past the last valid
	// offset. Reclamp before the minimal-scroll branches.
	if maxOffset := b.viewport.TotalLineCount() - b.viewport.Height; b.viewport.YOffset > maxOffset {
		b.viewport.SetYOffset(max(0, maxOffset))
	}
	sp, ok := b.logRowSpans[b.logCursorSeq]
	if !ok {
		// Spans missing (should not happen after updateViewport): fall back to
		// the pre-C4 1-entry==1-row math.
		if b.logCursorIdx < b.viewport.YOffset {
			b.viewport.SetYOffset(b.logCursorIdx)
		} else if b.logCursorIdx >= b.viewport.YOffset+b.viewport.Height {
			b.viewport.SetYOffset(b.logCursorIdx - b.viewport.Height + 1)
		}
		return
	}
	if sp.Last < b.viewport.YOffset {
		b.viewport.SetYOffset(sp.First)
	} else if sp.First >= b.viewport.YOffset+b.viewport.Height {
		b.viewport.SetYOffset(sp.Last - b.viewport.Height + 1)
	}
}

// logSearchMatchInfo derives the status-bar match indicator for the current
// logSearchQuery over entries (the filtered list): total match count and the
// cursor's 1-based position among matches (0 when the cursor is off any match).
// Mirrors requestSearchMatchInfo; entries is passed in so statusBar shares one
// filteredEntries() scan with the visible/total count (D10).
func (b *BaseModel) logSearchMatchInfo(entries []domain.LogEntry) (position, total int) {
	if b.logSearchQuery == "" {
		return 0, 0
	}
	for i, entry := range entries {
		if logMatchesSearch(entry, b.logSearchQuery) {
			total++
			if i == b.logCursorIdx {
				position = total
			}
		}
	}
	return position, total
}

// nearBottomThreshold is the scroll percentage (0.0-1.0) at which we consider
// the viewport to be "near" the bottom for auto-follow purposes.
const nearBottomThreshold = 0.98

// isNearBottom checks if the viewport is at or near the bottom
func (b *BaseModel) isNearBottom() bool {
	if b.viewport.AtBottom() {
		return true
	}
	return b.viewport.ScrollPercent() >= nearBottomThreshold
}

// setRequestCursor is the SOLE mutator of the requests-view cursor. It clamps
// idx into [0, len(requests)-1] and updates cursorIdx and cursorID together so
// the pair can never disagree; an empty list resets to the no-cursor sentinel
// (cursorIdx -1, cursorID ""). Every mover — nav keys and resolveRequestCursor —
// goes through here.
func (b *BaseModel) setRequestCursor(requests []proxy.RequestRecord, idx int) {
	if len(requests) == 0 {
		b.cursorIdx = -1
		b.cursorID = ""
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx > len(requests)-1 {
		idx = len(requests) - 1
	}
	b.cursorIdx = idx
	b.cursorID = requests[idx].ID
}

// resolveRequestCursor re-anchors the cursor against the current filtered list
// at the render choke point, so every mutation source (upsert, append+trim,
// live filter keystrokes, esc clear, follow toggles, view transitions) lands
// with a coherent cursor. Precedence:
//   - follow mode pins the cursor to the newest (last) row;
//   - else if cursorID is still present, its current index (in-place upserts and
//     appends never move the cursor off its row);
//   - else the stale cursorIdx is clamped and re-anchored to whatever row now
//     sits at that index (trim/filter removed the old row; empty→non-empty with
//     follow off lands on row 0).
func (b *BaseModel) resolveRequestCursor(requests []proxy.RequestRecord) {
	if len(requests) == 0 {
		b.setRequestCursor(requests, 0) // resets to the no-cursor sentinel
		return
	}
	if b.followMode {
		b.setRequestCursor(requests, len(requests)-1)
		return
	}
	if b.cursorID != "" { // "" is the no-cursor sentinel; never a real ID
		for i, req := range requests {
			if req.ID == b.cursorID {
				b.setRequestCursor(requests, i)
				return
			}
		}
	}
	// Stale cursor (row filtered/trimmed away) or first anchor with follow off:
	// clamp the last-known index and re-anchor to the row now there.
	b.setRequestCursor(requests, b.cursorIdx)
}

// ensureCursorVisible scrolls the viewport the minimum amount needed to bring
// the cursor row on-screen. Called from updateViewport (the SetContent choke
// point) so the marker is visible after EVERY transition — tab-in from a
// scrolled logs view, resize, append/trim, filter edit, detail-return — not
// just after a keypress.
func (b *BaseModel) ensureCursorVisible() {
	if b.cursorIdx < 0 {
		return // no-cursor sentinel (empty list)
	}
	// A grown viewport (or shrunk list) can leave YOffset past the last valid
	// offset — the cursor then sits "in range" of a window that shows blank
	// overscroll. Reclamp before the minimal-scroll branches.
	if maxOffset := b.viewport.TotalLineCount() - b.viewport.Height; b.viewport.YOffset > maxOffset {
		b.viewport.SetYOffset(max(0, maxOffset))
	}
	if b.cursorIdx < b.viewport.YOffset {
		b.viewport.SetYOffset(b.cursorIdx)
	} else if b.cursorIdx >= b.viewport.YOffset+b.viewport.Height {
		b.viewport.SetYOffset(b.cursorIdx - b.viewport.Height + 1)
	}
}

// clampViewportToContent pulls the viewport back when a content replacement
// left the scroll offset past the end — a capture-disabled final detail can be
// SHORTER than the in-flight view it replaces (the in-flight note vanishes and
// no body sections take its place), stranding a bottom-scrolled reader on
// blank overscroll.
func (b *BaseModel) clampViewportToContent() {
	if maxOffset := b.viewport.TotalLineCount() - b.viewport.Height; b.viewport.YOffset > maxOffset {
		b.viewport.SetYOffset(max(0, maxOffset))
	}
}

// updateViewport updates the viewport content
func (b *BaseModel) updateViewport() {
	var lines []string

	switch b.viewMode {
	case ViewModeRequestDetail:
		b.logRowSpans = nil
		lines = b.formatRequestDetail()
	case ViewModeRequests:
		b.logRowSpans = nil
		requests := b.filteredProxyRequests()
		// Resolve the cursor against the just-computed filtered list BEFORE
		// formatting so the marker (baked into content below) and the scroll
		// invariant agree within this single render (D6/D8).
		b.resolveRequestCursor(requests)
		for i, req := range requests {
			// Prefix the cursor row with a styled marker and every other row
			// with two spaces so columns stay aligned (D10).
			marker := "  "
			if i == b.cursorIdx {
				marker = s.Cursor.Render("❯ ")
			}
			lines = append(lines, marker+b.formatProxyRequest(req))
		}
	default: // ViewModeLogs
		entries := b.filteredEntries()
		// A `/`-search adds a cursor row marker (mirroring the requests block).
		// Resolve the Seq-anchored cursor against the just-computed list BEFORE
		// formatting so the marker index and formatLogEntry's inline highlight
		// agree within this single render. No cursor/marker without a search:
		// the marker prefix would otherwise shift every log line for no reason.
		searching := b.logSearchQuery != ""
		if searching {
			b.resolveLogCursor(entries)
		}
		// Rebuild DisplaySeq→row spans every render (Codex #2). Wrap is isolated
		// so the no-wrap path stays byte-identical to pre-C4.
		b.logRowSpans = make(map[int64]logRowSpan, len(entries))
		row := 0
		wrapOn := b.settings.Wrap
		wrapWidth := b.viewport.Width
		for i, entry := range entries {
			line := b.formatLogEntry(entry)
			if searching {
				marker := "  "
				if i == b.logCursorIdx {
					marker = s.Cursor.Render("❯ ")
				}
				line = marker + line
			}
			if wrapOn && wrapWidth > 0 {
				// Highlight before wrap — offsets stay valid on the unwrapped
				// string (plan 021 WS4). Requests never take this branch.
				wrapped := ansi.Wordwrap(line, wrapWidth, "")
				parts := strings.Split(wrapped, "\n")
				if len(parts) == 0 {
					parts = []string{""}
				}
				first := row
				for _, p := range parts {
					lines = append(lines, p)
					row++
				}
				b.logRowSpans[entry.DisplaySeq] = logRowSpan{First: first, Last: row - 1}
			} else {
				lines = append(lines, line)
				b.logRowSpans[entry.DisplaySeq] = logRowSpan{First: row, Last: row}
				row++
			}
		}
	}

	content := strings.Join(lines, "\n")
	b.viewport.SetContent(content)

	// Cursor visibility invariant for the requests view (D7). Runs after
	// SetContent so the marker is on-screen after every transition, not just
	// keypresses. Follow mode overrides to the bottom.
	//
	// Logs follow also GotoBottoms here (Codex #2): pre-C4 this was requests-
	// only and renderAfterLogEntries covered live arrivals, but wrap makes the
	// gap visible — SetContent can leave YOffset off the true bottom.
	switch b.viewMode {
	case ViewModeRequests:
		if b.followMode {
			b.viewport.GotoBottom()
		} else {
			b.ensureCursorVisible()
		}
	case ViewModeLogs:
		if b.followMode {
			b.viewport.GotoBottom()
		}
	}
}

// renderDetailFromTop re-renders the viewport and scrolls it to the top. It is
// the shared tail of both models' Enter-into-detail transitions: a detail
// opened from deep in the list would otherwise inherit the list's YOffset. The
// GotoTop must follow updateViewport (the detail-view render deliberately never
// touches YOffset) and must stay out of updateViewport itself, which also runs
// on background arrivals where re-scrolling would yank the reader.
func (b *BaseModel) renderDetailFromTop() {
	b.updateViewport()
	b.viewport.GotoTop()
}

// formatRequestDetail formats the request detail view
func (b *BaseModel) formatRequestDetail() []string {
	var lines []string

	if b.detailLoading {
		lines = append(lines, "Loading request details...")
		return lines
	}

	if b.detailError != nil {
		lines = append(lines, s.Err.Render("Error: "+b.detailError.Error()))
		return lines
	}

	if b.requestDetail == nil {
		lines = append(lines, "No request selected")
		return lines
	}

	d := b.requestDetail

	// Header info
	lines = append(lines, s.Header.Render(fmt.Sprintf("Request: %s", d.ID)))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  Time:     %s", d.Timestamp))
	lines = append(lines, fmt.Sprintf("  Method:   %s", d.Method))
	lines = append(lines, fmt.Sprintf("  URL:      %s", d.URL))
	lines = append(lines, fmt.Sprintf("  Status:   %d", d.StatusCode))
	switch {
	case d.InFlight && d.Stale:
		lines = append(lines, "  Duration: (in flight, stale?)")
	case d.InFlight:
		lines = append(lines, "  Duration: (in flight)")
	default:
		lines = append(lines, fmt.Sprintf("  Duration: %dms", d.DurationMs))
	}
	lines = append(lines, fmt.Sprintf("  Remote:   %s", d.RemoteAddr))

	// Details arrive only on completion: an in-flight record is guaranteed
	// nil Details (see proxy.RequestRecord.InFlight), so it never has
	// headers/bodies to render here. That's expected, not a "capture
	// disabled" state, so it gets its own note instead of silently
	// rendering nothing.
	if d.InFlight {
		lines = append(lines, "")
		switch {
		case b.detailRefreshFailed:
			// A live-refresh attempt (attach mode — D16) failed while this
			// in-flight snapshot was on screen: say so instead of silently
			// re-promising details that may never arrive automatically.
			lines = append(lines, s.Dim.Render("(live refresh failed — press esc and re-enter to reload)"))
		case d.Stale:
			// D8, #53: the completion event may have been lost — the true
			// outcome is unknown, not necessarily broken (long-lived
			// streams/transfers can legitimately still be live here).
			lines = append(lines, s.Dim.Render("(request in flight, stale? — the completion event may have been lost; true outcome unknown)"))
		default:
			lines = append(lines, s.Dim.Render("(request in flight — details arrive on completion)"))
		}
	}

	// Request headers
	if len(d.RequestHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, s.Header.Render("Request Headers"))
		for name, values := range d.RequestHeaders {
			for _, value := range values {
				lines = append(lines, fmt.Sprintf("  %s: %s", s.Dim.Render(name), value))
			}
		}
	}

	// Response headers
	if len(d.ResponseHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, s.Header.Render("Response Headers"))
		for name, values := range d.ResponseHeaders {
			for _, value := range values {
				lines = append(lines, fmt.Sprintf("  %s: %s", s.Dim.Render(name), value))
			}
		}
	}

	// Request body
	lines = append(lines, renderBodySection("Request Body", d.RequestBody)...)

	// Response body
	lines = append(lines, renderBodySection("Response Body", d.ResponseBody)...)

	// Footer hint
	lines = append(lines, "")
	lines = append(lines, s.Dim.Render("Press ESC to go back"))

	return lines
}

// renderBodySection renders one titled request/response body block: a header
// line carrying size/Content-Type/Content-Encoding/truncation, followed by
// the body content. Shared by both Model and ClientModel via BaseModel, so
// this lands the rendering behavior once for both. Returns nil (nothing
// rendered) when there is no body or it is empty, matching prior behavior.
func renderBodySection(title string, b *BodyData) []string {
	if b == nil || b.Size == 0 {
		return nil
	}
	lines := []string{"", s.Header.Render(bodySectionTitle(title, b))}
	return append(lines, renderBodyLines(b)...)
}

// bodySectionTitle builds the section header, e.g.
// "Request Body (35 bytes, application/json)" or
// "Response Body (1234 bytes, application/json, gzip, truncated)". The
// Content-Type and Content-Encoding segments are omitted when absent, and no
// dangling comma/parens are left behind when every optional segment is empty.
func bodySectionTitle(title string, b *BodyData) string {
	parts := []string{fmt.Sprintf("%d bytes", b.Size)}
	if b.ContentType != "" {
		parts = append(parts, b.ContentType)
	}
	if b.ContentEncoding != "" {
		parts = append(parts, b.ContentEncoding)
	}
	if b.Truncated {
		parts = append(parts, "truncated")
	}
	return fmt.Sprintf("%s (%s)", title, strings.Join(parts, ", "))
}

// renderBodyLines renders the content lines for a captured body: an
// unavailable (evicted) notice, a bounded hexdump preview for binary data, or
// the body text split into lines. Non-binary JSON bodies (Content-Type
// contains "json", or the raw text is itself valid JSON) are pretty-printed
// 2-space indented; any json.Indent failure falls back to the raw text. Text
// otherwise renders unchanged except that ASCII control characters (< 0x20,
// other than tab and the newlines used for line splitting) and DEL (0x7F) are
// replaced with the Unicode replacement character, so ESC/BEL/OSC sequences
// from a captured body cannot manipulate the terminal. (Classification
// usually marks such bodies binary, but a socket-supplied record could lie;
// this is a cheap defense.)
func renderBodyLines(body *BodyData) []string {
	if body.Unavailable {
		return []string{s.Dim.Render("(body no longer available)")}
	}
	if body.IsBinary {
		return renderBinaryPreview(body.Data, body.DataBase64)
	}
	if body.Data == "" {
		return nil
	}

	text := body.Data
	if shouldPrettyPrintJSON(body) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(body.Data), "", "  "); err == nil {
			text = buf.String()
		}
	}

	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, "  "+sanitizeControlChars(line))
	}
	return lines
}

// sanitizeControlChars replaces ASCII control characters that could manipulate
// the terminal (everything < 0x20 except tab, plus DEL 0x7F) with the Unicode
// replacement character. Newlines are already consumed by line splitting before
// this runs, so only intra-line control bytes (ESC, BEL, etc.) are affected.
func sanitizeControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return '�'
		}
		return r
	}, s)
}

// hexPreviewMaxBytes caps how many leading bytes of a binary body are
// hex-dumped in the detail view (D11, #50.2): enough to identify the format
// (magic bytes, headers) without flooding the terminal with a large payload.
const hexPreviewMaxBytes = 256

// hexPreviewBytesPerLine is the hexdump -C convention of 16 bytes per line,
// split into two 8-byte groups.
const hexPreviewBytesPerLine = 16

// renderBinaryPreview turns a binary body's Data into hexdump-style preview
// lines, dimming the trailing "more bytes" line. The API always base64-encodes
// binary bytes for the wire (client.go fetchRequestDetail -> clientBodyToBodyData),
// so DataBase64 is normally true here; dataBase64 false (raw, not base64-encoded
// bytes) is also supported — a decode failure falls back to treating Data as
// the raw bytes themselves, so both are previewed correctly. Empty raw bytes
// (nothing to preview, e.g. a body flagged binary but not actually loaded)
// fall back to the original placeholder rather than an empty hexdump.
func renderBinaryPreview(data string, dataBase64 bool) []string {
	raw := []byte(data)
	if dataBase64 {
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err == nil {
			raw = decoded
		}
	}
	if len(raw) == 0 {
		return []string{s.Dim.Render("[binary data]")}
	}

	lines := hexPreviewLines(raw, hexPreviewMaxBytes)
	if len(raw) > hexPreviewMaxBytes && len(lines) > 0 {
		lines[len(lines)-1] = s.Dim.Render(lines[len(lines)-1])
	}
	return lines
}

// hexPreviewLines renders up to maxBytes of data as an `hexdump -C`-style
// preview: an 8-digit offset, two 8-byte hex groups (an extra space between
// the groups), and an ASCII gutter between pipes where only printable ASCII
// (0x20-0x7E) renders as itself and everything else renders as '.' — the
// gutter never emits a raw control byte, the same terminal-safety discipline
// as sanitizeControlChars. When data is longer than maxBytes, only the first
// maxBytes are dumped and a final "(… N more bytes)" line reports the
// remainder (styling that line, if desired, is the caller's job — this
// function returns plain text so it stays a pure, golden-testable mapping
// from bytes to lines).
func hexPreviewLines(data []byte, maxBytes int) []string {
	preview := data
	remaining := 0
	if len(data) > maxBytes {
		preview = data[:maxBytes]
		remaining = len(data) - maxBytes
	}

	var lines []string
	for offset := 0; offset < len(preview); offset += hexPreviewBytesPerLine {
		end := offset + hexPreviewBytesPerLine
		if end > len(preview) {
			end = len(preview)
		}
		lines = append(lines, formatHexLine(offset, preview[offset:end]))
	}
	if remaining > 0 {
		lines = append(lines, fmt.Sprintf("(… %d more bytes)", remaining))
	}
	return lines
}

// formatHexLine renders one hexdump -C line for a chunk of 1-16 bytes at the
// given offset: "OFFSET  HEXGROUP1 HEXGROUP2 |ASCII|". Short chunks (the
// final line of a body whose length isn't a multiple of 16) pad the hex
// groups with blanks to keep the ASCII gutter aligned, but the ASCII gutter
// itself only shows the bytes actually present (matching hexdump -C).
func formatHexLine(offset int, chunk []byte) string {
	first := chunk
	var second []byte
	if len(chunk) > hexPreviewBytesPerLine/2 {
		first = chunk[:hexPreviewBytesPerLine/2]
		second = chunk[hexPreviewBytesPerLine/2:]
	}

	var ascii strings.Builder
	for _, b := range chunk {
		if b >= 0x20 && b <= 0x7e {
			ascii.WriteByte(b)
		} else {
			ascii.WriteByte('.')
		}
	}

	return fmt.Sprintf("%08x  %s %s |%s|", offset, hexGroup(first), hexGroup(second), ascii.String())
}

// hexGroup renders up to 8 bytes as "%02x " each, padding absent slots with
// "   " so hex columns stay aligned regardless of how many bytes are present.
func hexGroup(b []byte) string {
	var sb strings.Builder
	for i := 0; i < hexPreviewBytesPerLine/2; i++ {
		if i < len(b) {
			fmt.Fprintf(&sb, "%02x ", b[i])
		} else {
			sb.WriteString("   ")
		}
	}
	return sb.String()
}

// shouldPrettyPrintJSON reports whether a non-binary body should be run
// through json.Indent before display: either its Content-Type declares JSON,
// or (no such declaration) the raw text happens to be valid JSON on its own.
func shouldPrettyPrintJSON(body *BodyData) bool {
	if strings.Contains(strings.ToLower(body.ContentType), "json") {
		return true
	}
	return json.Valid([]byte(body.Data))
}

// filteredEntries returns log entries after applying filters
func (b *BaseModel) filteredEntries() []domain.LogEntry {
	var result []domain.LogEntry

	for _, entry := range b.logEntries {
		// Process filter
		if b.soloProcess != "" && entry.Process != b.soloProcess {
			continue
		}

		// Check filterProcesses map
		if show, ok := b.filterProcesses[entry.Process]; ok && !show {
			continue
		}

		// String filter
		if b.searchPattern != "" {
			if !containsIgnoreCase(entry.Line, b.searchPattern) {
				continue
			}
		}

		result = append(result, entry)
	}

	return result
}

// filteredProxyRequests returns proxy requests after applying filters. Sized up
// front for the no-match-dropped case: this runs two or three times per
// keypress (cursor move, viewport rebuild, status bar) over a list that now
// reaches constants.MaxProxyRequests, and regrowing from nil each time is the
// bulk of that cost.
func (b *BaseModel) filteredProxyRequests() []proxy.RequestRecord {
	result := make([]proxy.RequestRecord, 0, len(b.proxyRequests))

	for _, req := range b.proxyRequests {
		// String filter (on URL, method, and subdomain)
		if b.searchPattern != "" {
			matchesURL := containsIgnoreCase(req.URL, b.searchPattern)
			matchesMethod := containsIgnoreCase(req.Method, b.searchPattern)
			matchesSubdomain := containsIgnoreCase(req.Subdomain, b.searchPattern)
			if !matchesURL && !matchesMethod && !matchesSubdomain {
				continue
			}
		}

		result = append(result, req)
	}

	return result
}

// getSelectedRequest returns the ID of the cursor row in the requests view,
// which Enter opens. Returns "" outside the requests view or when the list is
// empty (the no-cursor sentinel). The cursor is kept current by
// resolveRequestCursor at every render, so cursorID is always a live row's ID.
func (b *BaseModel) getSelectedRequest() string {
	if b.viewMode != ViewModeRequests {
		return ""
	}
	return b.cursorID
}

// formatProxyRequest formats a single proxy request for display
func (b *BaseModel) formatProxyRequest(req proxy.RequestRecord) string {
	// Format timestamp
	ts := req.Timestamp.Format("15:04:05")

	// Format subdomain with padding
	subdomain := fmt.Sprintf("%-10s", req.Subdomain)

	// Format method with padding (7 chars to accommodate DELETE/OPTIONS)
	method := fmt.Sprintf("%-7s", req.Method)

	// Format status with color based on status code
	codeStyle := s.HTTPSuccess
	switch {
	case req.StatusCode < 100:
		codeStyle = s.Dim // Gray for unknown/error (status 0)
	case req.StatusCode < 200:
		codeStyle = s.Dim // Gray for informational 1xx
	case req.StatusCode >= 500:
		codeStyle = s.HTTPError
	case req.StatusCode >= 400:
		codeStyle = s.HTTPWarning
	case req.StatusCode >= 300:
		codeStyle = s.HTTPRedirect
	}
	status := codeStyle.Render(fmt.Sprintf("%3d", req.StatusCode))

	// Format the duration column, including its "ms" unit, in one piece: the
	// normal/in-flight/overflow cases share a digits-or-filler value plus
	// "ms". A stale in-flight row (D8, #53: the completion event may have
	// been lost, true outcome unknown — not necessarily broken, long-lived
	// streams/transfers can legitimately still be live here) renders
	// "stale?" instead, which isn't a duration so it carries no "ms" suffix.
	durationMs := req.Duration.Milliseconds()
	var duration string
	switch {
	case b.requestIsStale(req):
		duration = "stale?"
	case req.InFlight:
		duration = "  ...ms"
	case durationMs > 9999:
		duration = "9999+ms"
	default:
		duration = fmt.Sprintf("%5dms", durationMs)
	}

	return fmt.Sprintf("%s  %s  %s %s %s  %s",
		s.Dim.Render(ts),
		s.Dim.Render(subdomain),
		method,
		status,
		s.Dim.Render(duration),
		req.URL,
	)
}

// getProcessStyle returns the style for a process name
func getProcessStyle(name string, processes []domain.ProcessInfo) lipgloss.Style {
	// Find process index for color
	for i, p := range processes {
		if p.Name == name {
			return s.ProcessColors[i%len(s.ProcessColors)]
		}
	}
	return s.DefaultProcess
}

// formatLogEntry formats a single log entry for display
func (b *BaseModel) formatLogEntry(entry domain.LogEntry) string {
	// Get process color
	procStyle := getProcessStyle(entry.Process, b.processes)

	// Format process name with padding
	procName := fmt.Sprintf("%-10s", entry.Process)

	// Build line
	prefix := procStyle.Render(procName)

	// Stream indicator
	streamIndicator := ""
	if entry.Stream == domain.StreamStderr {
		streamIndicator = s.Err.Render(" ERR ")
	}

	line := b.highlightLogLine(entry.Line)
	// Timestamps toggle (plan 021 WS4): omitting the fixed-width `15:04:05 `
	// prefix shifts the process-name column left — intentional, no padding
	// compensation. Default true preserves today's always-on rendering.
	if b.settings.Timestamps {
		ts := s.Dim.Render(entry.Timestamp.Format("15:04:05"))
		return fmt.Sprintf("%s %s%s %s", ts, prefix, streamIndicator, line)
	}
	return fmt.Sprintf("%s%s %s", prefix, streamIndicator, line)
}

// highlightLogLine wraps the first case-insensitive match of the active
// logSearchQuery in line with s.SearchHighlight (D9). It highlights only
// when BOTH the query and the WHOLE line are plain ASCII with no ESC byte:
// case-folding an ASCII line preserves byte offsets (so the styled run lands
// exactly on the match), and excluding ESC keeps a digit query from matching
// inside an ANSI escape sequence. Any other line (unicode, or one already
// carrying escape codes) falls back to the unstyled text — the row marker alone
// signals the match. Returns line unchanged when no search is active or nothing
// matches.
func (b *BaseModel) highlightLogLine(line string) string {
	q := b.logSearchQuery
	if q == "" {
		return line
	}
	if !isASCIINoESC(q) || !isASCIINoESC(line) {
		return line
	}
	// Both sides are ASCII, so ToLower does not shift byte offsets: the index
	// found in the lowered line is valid in the original.
	idx := strings.Index(strings.ToLower(line), strings.ToLower(q))
	if idx < 0 {
		return line
	}
	end := idx + len(q)
	return line[:idx] + s.SearchHighlight.Render(line[idx:end]) + line[end:]
}

// isASCIINoESC reports whether s is pure ASCII (every byte < 0x80) and contains
// no ESC byte (0x1b). It gates the inline search highlight: non-ASCII text
// breaks the byte-offset-preserving case fold, and an embedded ESC means a
// match could split an ANSI escape sequence (see highlightLogLine).
func isASCIINoESC(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 || s[i] == 0x1b {
			return false
		}
	}
	return true
}

// processStyle returns style based on process state
func processStyle(state domain.ProcessState) lipgloss.Style {
	switch state {
	case domain.ProcessStateRunning:
		return s.Running
	case domain.ProcessStateStopped:
		return s.Stopped
	case domain.ProcessStateCrashed:
		return s.Crashed
	case domain.ProcessStateStarting:
		return s.Starting
	case domain.ProcessStateStopping:
		return s.Stopping
	case domain.ProcessStateWaiting:
		return s.Waiting
	case domain.ProcessStateBlocked:
		return s.Blocked
	case domain.ProcessStateCompleted:
		return s.Completed
	default:
		return s.DefaultProcess
	}
}

// gatedDetail returns the inline gated-launch annotation for a process (plan 013
// D5): " (waiting on: X, Y)" while waiting, " (blocked on: X)" while blocked, and
// "" in every other state. Targets are shown in declaration order.
func gatedDetail(p domain.ProcessInfo) string {
	switch p.State {
	case domain.ProcessStateWaiting:
		if len(p.WaitingOn) > 0 {
			return " (waiting on: " + strings.Join(p.WaitingOn, ", ") + ")"
		}
	case domain.ProcessStateBlocked:
		if len(p.BlockedOn) > 0 {
			return " (blocked on: " + strings.Join(p.BlockedOn, ", ") + ")"
		}
	}
	return ""
}

// healthDot returns the process panel's health indicator (plan 018 D13): a
// green " ●" for domain.HealthStatusHealthy, a red " ✗" for
// domain.HealthStatusUnhealthy, and "" for domain.HealthStatusUnknown or any
// other/empty value — so a process with no healthcheck configured renders
// nothing extra. The two states use distinct GLYPHS, not just colors: on a
// NO_COLOR/monochrome terminal, or to a red-green color-blind reader, two
// same-shaped dots are indistinguishable (CodeRabbit, PR #88).
func healthDot(status domain.HealthStatus) string {
	switch status {
	case domain.HealthStatusHealthy:
		return s.HealthyDot.Render(" ●")
	case domain.HealthStatusUnhealthy:
		return s.UnhealthyDot.Render(" ✗")
	default:
		return ""
	}
}

// processPanel renders the process status header. Content is constrained to
// frame width with an ANSI-aware cut so it cannot terminal-wrap unpredictably
// (Codex #8).
func (b *BaseModel) processPanel() string {
	var items []string

	// Show processes panel in both views
	for i, proc := range b.processes {
		style := processStyle(proc.State)

		// Highlight if solo'd (only in logs view)
		name := proc.Name
		if b.viewMode == ViewModeLogs && b.soloProcess == proc.Name {
			name = fmt.Sprintf("[%s]", proc.Name)
		}

		// Gated-launch detail (plan 013 D5): a waiting/blocked process shows what
		// it is gated on inline, in declaration order, so the panel explains why it
		// has not launched. Kept minimal — this compact panel has no separate
		// detail area.
		name += gatedDetail(proc)

		// Health dot (plan 018 D13): appended as a separately styled segment
		// after the name so the name's state style cannot swallow or recolor
		// it. Empty for unknown/unset health, so processes without a
		// healthcheck render byte-identical to before this feature.
		dot := healthDot(proc.Health)

		// Show number key (only in logs view where 1-9 keys work)
		if b.viewMode == ViewModeLogs {
			key := fmt.Sprintf("%d:", i+1)
			items = append(items, style.Render(key+name)+dot)
		} else {
			items = append(items, style.Render(name)+dot)
		}
	}

	header := lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(items, "  "))
	if b.width > 0 {
		// Header Padding(0,1) consumes 2 columns; cut so Render fits the frame.
		inner := b.width - 2
		if inner < 0 {
			inner = 0
		}
		if ansi.StringWidth(header) > inner {
			header = ansi.Cut(header, 0, inner)
		}
		return s.Header.Width(b.width).MaxWidth(b.width).Render(header)
	}
	return s.Header.Render(header)
}

// statusLeftDefault builds the left status-bar text for normal mode (no input
// prompt active). requests is the requests-view filtered list (unused in the
// logs view), passed in so callers can share one filteredProxyRequests() scan
// with the visible/total count. Precedence differs by view (D13):
//   - Requests view: the `/` search indicator wins when a query is set —
//     "/<query> (i/k)" with i the cursor's 1-based position among matches when
//     the cursor is on a match, else "/<query> (k matches)" (0 included) — with
//     "| filter: <pattern>" appended when the `s` filter is also active. soloProcess
//     is a logs-view concept and is never shown here.
//   - Logs view: the `/` search indicator wins when a query is set —
//     "/<query> (i/k)" (cursor on match i of k) or "/<query> (k matches)" when
//     the cursor is off any match — then solo, then the `s` filter (D10). The
//     `s` filter and search compose (search navigates within the filtered set).
//   - Otherwise (either view): the `s` filter line, then the default hint.
func (b *BaseModel) statusLeftDefault(extraInfo string, requests []proxy.RequestRecord, entries []domain.LogEntry) string {
	if b.viewMode == ViewModeRequests && b.requestSearchQuery != "" {
		position, total := b.requestSearchMatchInfo(requests)
		var indicator string
		if position > 0 {
			indicator = fmt.Sprintf("/%s (%d/%d)", b.requestSearchQuery, position, total)
		} else {
			indicator = fmt.Sprintf("/%s (%d matches)", b.requestSearchQuery, total)
		}
		if b.searchPattern != "" {
			indicator += fmt.Sprintf(" | filter: %s", b.searchPattern)
		}
		return indicator
	}
	if b.viewMode == ViewModeLogs && b.logSearchQuery != "" {
		position, total := b.logSearchMatchInfo(entries)
		if position > 0 {
			return fmt.Sprintf("/%s (%d/%d)", b.logSearchQuery, position, total)
		}
		return fmt.Sprintf("/%s (%d matches)", b.logSearchQuery, total)
	}
	if b.viewMode != ViewModeRequests && b.soloProcess != "" {
		return fmt.Sprintf("Showing: %s (ESC to clear)", b.soloProcess)
	}
	if b.searchPattern != "" {
		return fmt.Sprintf("Filter: %s (ESC to clear)", b.searchPattern)
	}
	return b.statusDefaultHint(extraInfo)
}

// statusDefaultHint is the fallback status-bar hint shown when no filter, search,
// or solo is active.
func (b *BaseModel) statusDefaultHint(extraInfo string) string {
	hint := "Tab: switch view | ? for help"
	if extraInfo != "" {
		hint += " | " + extraInfo
	}
	return hint
}

// statusBar renders the bottom status bar
func (b *BaseModel) statusBar(extraInfo string) string {
	var left, right string

	// View mode indicator
	viewIndicator := "[Logs]"
	switch b.viewMode {
	case ViewModeRequests:
		viewIndicator = "[Requests]"
	case ViewModeRequestDetail:
		viewIndicator = "[Request Detail]"
	}

	// Filtered lists, each computed once and shared below between the left-side
	// search indicator and the right-side visible count so a single render never
	// rescans the underlying slice twice. Only the active view's list is built.
	var requests []proxy.RequestRecord
	var entries []domain.LogEntry
	if b.viewMode == ViewModeRequests {
		requests = b.filteredProxyRequests()
	} else {
		entries = b.filteredEntries()
	}

	// Left side: mode/filter info
	switch b.mode {
	case ModeFilter:
		left = "Filter: " + b.textInput.View()
	case ModeSearch:
		left = "Search: " + b.textInput.View()
	case ModeStringFilter:
		left = "String filter: " + b.textInput.View()
	default:
		left = b.statusLeftDefault(extraInfo, requests, entries)
	}

	// Per-stream health and scroll-back state ride on the end of the left side in
	// every mode: a degraded stream (or a page still loading) is worth showing
	// even while a filter prompt is open, and both are orthogonal to the
	// overall-connection text attach mode owns.
	if segs := append(b.streamHealthSegments(), b.requestsPagingSegments()...); len(segs) > 0 {
		left += " | " + strings.Join(segs, " | ")
	}

	// Right side: follow mode and count
	var visible, total int
	var label string
	if b.viewMode == ViewModeRequests {
		visible = len(requests)
		total = len(b.proxyRequests)
		label = "requests"
	} else {
		visible = len(entries)
		total = len(b.logEntries)
		label = "lines"
	}
	followIndicator := "[FOLLOW]"
	if !b.followMode {
		followIndicator = "[PAUSED]"
	}
	right = fmt.Sprintf("%s %s %d/%d %s", viewIndicator, followIndicator, visible, total, label)

	// Calculate widths. Use display width (lipgloss.Width), never byte len: a
	// Unicode search query in `left` would misplace the right-aligned counts if
	// the layout math counted bytes (D13).
	leftWidth := b.width - lipgloss.Width(right) - 4
	if leftWidth < 0 {
		leftWidth = 0
	}

	// Force a single row: lipgloss wraps by default when Width is set, which
	// would blow the chrome height budget (relayout / Codex #8).
	leftPart := s.Status.Width(leftWidth).MaxHeight(1).Render(left)
	rightPart := s.Status.MaxHeight(1).Render(right)

	return lipgloss.JoinHorizontal(lipgloss.Top, leftPart, "  ", rightPart)
}

// mainView renders the main TUI layout. Chrome rows are derived from settings
// (relayout / Codex #8); the open dropdown is spliced over the composed frame
// (overlay.go / Codex #1).
func (b *BaseModel) mainView(extraStatusInfo string) string {
	b.clearMenuHitRects()

	var lines []string

	if b.settings.MenuBar {
		lines = append(lines, b.renderMenuBar())
	}

	if b.settings.ProcessPanel {
		panel := b.processPanel()
		for _, line := range strings.Split(panel, "\n") {
			lines = append(lines, padFrameRow(line, b.width))
		}
	}

	for _, line := range strings.Split(b.viewport.View(), "\n") {
		lines = append(lines, padFrameRow(line, b.width))
	}

	lines = append(lines, padFrameRow(singleFrameLine(b.statusBar(extraStatusInfo)), b.width))
	lines = append(lines, b.renderKeyHints())

	if b.menuOpen() {
		lines = b.applyMenuOverlay(lines)
	}

	return strings.Join(lines, "\n")
}

// singleFrameLine keeps the first visual row of s (defensive against a chrome
// helper that still wraps at pathological widths).
func singleFrameLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// helpView renders the help overlay based on current view mode
func (b *BaseModel) helpView() string {
	if b.viewMode == ViewModeRequests {
		return b.requestsHelpView()
	}
	return b.logsHelpView()
}

// logsHelpView renders the help overlay for logs view
func (b *BaseModel) logsHelpView() string {
	title := "Prox - Process Manager"
	if b.helpConfig.TitleSuffix != "" {
		title += " " + b.helpConfig.TitleSuffix
	}
	title += " [Logs View]"

	quitMsg := "Quit"
	if b.helpConfig.QuitMessage != "" {
		quitMsg = b.helpConfig.QuitMessage
	}

	help := fmt.Sprintf(`
%s

Views:
  Tab        Switch to Requests view

Navigation:
  j/↓        Scroll down
  k/↑        Scroll up (pauses auto-follow)
  g/Home     Go to top (pauses auto-follow)
  G/End      Go to bottom (resumes auto-follow)
  PgUp/PgDn  Page up/down
  F          Toggle auto-follow mode

Search (jumps the cursor, does NOT filter):
  /          Search lines (jump to match at/after the current position)
  n/N        Jump to next/previous match (wraps)

Filtering:
  1-9        Solo process (toggle)
  f          Filter mode (process selection)
  s          Substring filter (hide non-matching, applied live)
  ESC        Clear filters and search

Other:
  r          Restart selected process (1-9 to select)
  ?          Toggle help
  q/Ctrl+C   %s

Press any key to close help...
`, title, quitMsg)

	return s.Help.Render(help)
}

// requestsHelpView renders the help overlay for requests view
func (b *BaseModel) requestsHelpView() string {
	title := "Prox - Process Manager"
	if b.helpConfig.TitleSuffix != "" {
		title += " " + b.helpConfig.TitleSuffix
	}
	title += " [Requests View]"

	quitMsg := "Quit"
	if b.helpConfig.QuitMessage != "" {
		quitMsg = b.helpConfig.QuitMessage
	}

	help := fmt.Sprintf(`
%s

Views:
  Tab        Switch to Logs view

Navigation (moves the cursor row; reaching the oldest row loads older requests):
  j/↓        Move cursor down (onto newest row resumes auto-follow)
  k/↑        Move cursor up (pauses auto-follow)
  g/Home     Move cursor to top (pauses auto-follow)
  G/End      Move cursor to bottom (resumes auto-follow)
  PgUp/PgDn  Move cursor half a page
  F          Toggle auto-follow mode

Search (jumps the cursor, does NOT filter):
  /          Search URL/method/subdomain (jump to match at/after cursor)
  n/N        Jump to next/previous match (wraps)

Request Details:
  Enter      View details for the cursor row
  ESC        Return to request list (or clear filters/search)

Filtering:
  s          String filter (URL/method/subdomain)
  ESC        Clear filters and search

Other:
  ?          Toggle help
  q/Ctrl+C   %s

Press any key to close help...
`, title, quitMsg)

	return s.Help.Render(help)
}

// containsIgnoreCase performs a case-insensitive substring search
func containsIgnoreCase(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// truncateError truncates an error message to maxLen characters
func truncateError(err error, maxLen int) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxLen {
		return msg[:maxLen-3] + "..."
	}
	return msg
}
