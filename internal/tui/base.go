package tui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
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
	soloProcess string // Single process to show (1-9 keys)

	// Per-view s-bar filter state (plan 021 WS6 / Codex #3). Grammars are
	// mutually incompatible, so each view keeps its own {RawQuery,LastGood,
	// ParseErr}. Filters persist across Tab; `/` search is untouched and
	// composes within the filtered list.
	logsFilter     logsFilterState
	requestsFilter requestsFilterState

	// requestSearchQuery is the requests-view `/` search term. It is DELIBERATELY
	// separate from the `s` filter: in the requests view `/` navigates (jumps the
	// cursor to matches) rather than filtering, so it composes with — never
	// overwrites — an active filter. Match state is never stored; n/N rescan
	// the filtered list at keypress time (D12/D13).
	requestSearchQuery string

	// logSearchQuery is the logs-view `/` search term. Like requestSearchQuery
	// (and unlike the `s` filter) it NAVIGATES rather than filters: `/` jumps
	// the logs cursor to the first matching line and n/N cycle, leaving every
	// line visible. Match state is never stored; the seek helpers rescan
	// filteredEntries() at keypress time (D6-D8).
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
	// statusFlashSeq is the flash generation: incremented by setStatusFlash
	// so a stale clear timer can't erase a newer flash (CodeRabbit PR #102).
	statusFlashSeq int

	// settings are persisted at ~/.prox/tui/config.toml (WS2). View bools drive
	// chrome (ProcessPanel/MenuBar) and log rendering (Timestamps/Wrap) — C4.
	settings Settings

	// projectName is shown in the menu bar (WS3). Set from ClientOptions or
	// resolved to the cwd base in RunClient.
	projectName string

	// Menu bar open state (WS3). openMenu is -1 when closed, otherwise a MenuID.
	// menuHighlight is the full-list index of the highlighted dropdown row.
	// menuWindow is the first visible item index — reset on open/sibling slide,
	// follows the highlight (see deriveMenuWindowStart).
	// Hit-rects live in hits (shared across View value-copies — plan 022 WS0)
	// and are cleared on close (strix stale-rect discipline / Codex #1).
	openMenu      int
	menuHighlight int
	menuWindow    int
	hits          *hitRegistry

	// logRowSpans maps DisplaySeq → display-row span in the logs viewport
	// content. Rebuilt every updateViewport (plan 021 WS4 / Codex #2): when wrap
	// is off the span is identity (entry i → {i,i} in filtered order); when wrap
	// is on a long line spans multiple rows. Search origin and cursor visibility
	// translate through these spans so 1-entry==1-row is no longer assumed.
	logRowSpans map[int64]logRowSpan

	// logMeta caches ingest-time level + JSON shape per DisplaySeq (plan 021 WS7).
	// Pruned in appendLogEntry when the ring drops entries (Codex #11).
	logMeta map[int64]logMeta

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

	// Mouse double-click on a requests row (plan 021 WS11 / Codex #5).
	lastRequestClickIdx int
	lastRequestClickAt  time.Time

	// helpOffset scrolls the help modal body when it exceeds the modal inner
	// height (clampHelpOffset on the real model; reset on open/close).
	helpOffset int
}

// newBaseModel creates a new BaseModel with the given help configuration
func newBaseModel(helpConfig HelpConfig) BaseModel {
	ti := textinput.New()
	ti.Placeholder = "Type to filter..."
	ti.CharLimit = 100
	ti.Width = 40
	applyTextInputTheme(&ti)

	b := BaseModel{
		processes:           make([]domain.ProcessInfo, 0),
		logEntries:          make([]domain.LogEntry, 0),
		proxyRequests:       make([]proxy.RequestRecord, 0),
		textInput:           ti,
		mode:                ModeNormal,
		viewMode:            ViewModeLogs,
		streamHealth:        make(map[StreamID]stream.Status),
		streamDropped:       make(map[StreamID]bool),
		followMode:          true,
		logCursorIdx:        -1, // no-cursor sentinel (pairs with logCursorSeq 0)
		lastRequestClickIdx: -1,
		helpConfig:          helpConfig,
		settings:            DefaultSettings(),
		openMenu:            -1, // closed
		hits:                &hitRegistry{},
	}
	b.viewport.MouseWheelEnabled = false // TUI owns all wheel routing (Codex #5)
	return b
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
	b.clampMenuWindow()
	// Clamp help on resize so the first k after a shrink works immediately
	// (plan 022 WS4 / PR #102 lesson — never clamp only in View).
	if b.mode == ModeHelp {
		b.clampHelpOffset()
	}
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
	b.viewport.MouseWheelEnabled = false // TUI owns wheel routing (Codex #5)
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

	if b.logMeta == nil {
		b.logMeta = make(map[int64]logMeta)
	}
	level, hasLevel := classifyLevel(entry.Line)
	b.logMeta[entry.DisplaySeq] = logMeta{
		level:    level,
		hasLevel: hasLevel,
		isJSON:   isJSONObject(entry.Line),
	}

	b.logEntries = append(b.logEntries, entry)
	// Keep only last entries - create new slice to release memory from old entries
	if len(b.logEntries) > maxLogEntries {
		// Drop meta for evicted entries before the slice copy (Codex #11).
		drop := b.logEntries[:len(b.logEntries)-maxLogEntries]
		for _, e := range drop {
			delete(b.logMeta, e.DisplaySeq)
		}
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
			// to requestSearchQuery (`s` filter untouched) and jump the cursor
			// to the first match at-or-after it (D12).
			b.requestSearchQuery = b.textInput.Value()
			b.jumpToRequestSearchMatch()
			b.updateViewport()
			return true, nil
		}
		// Logs view: `/` is navigation, not filtration (D6/D8) — it mirrors the
		// requests view. Commit the query to logSearchQuery (`s` filter
		// untouched) and jump the cursor to the first match at-or-after the
		// current position, wrapping. The scroll-to-match is a one-shot here
		// rather than wired into updateViewport, which also runs on streaming
		// arrivals and free j/k scroll where re-scrolling would fight the reader.
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

// handleStringFilterKey handles keys in string filter mode. Esc clears the
// ACTIVE view's filter (raw + expr) and exits; Enter exits keeping the query;
// every other key live-reparses the active RawQuery (plan 021 WS6 / Codex #3).
func (b *BaseModel) handleStringFilterKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		b.mode = ModeNormal
		b.textInput.Blur()
		b.clearActiveFilter()
		b.updateViewport()
		return true, nil

	case "enter":
		// Keep the query already applied by live-reparse; just exit the bar.
		b.applyActiveFilterQuery(b.textInput.Value())
		b.mode = ModeNormal
		b.textInput.Blur()
		b.updateViewport()
		return true, nil
	}

	var cmd tea.Cmd
	b.textInput, cmd = b.textInput.Update(msg)
	b.applyActiveFilterQuery(b.textInput.Value())
	b.updateViewport()
	return true, cmd
}

// applyActiveFilterQuery sets the active view's RawQuery and live-reparses.
// On success LastGood updates; on ParseErr LastGood is retained so mid-typing
// invalid queries keep the prior filter applied.
func (b *BaseModel) applyActiveFilterQuery(q string) {
	switch b.viewMode {
	case ViewModeRequests, ViewModeRequestDetail:
		b.requestsFilter.RawQuery = q
		expr, err := ParseRequestsFilter(q)
		b.requestsFilter.ParseErr = err
		if err == nil {
			b.requestsFilter.LastGood = expr
		}
	default:
		b.logsFilter.RawQuery = q
		expr, err := ParseLogsFilter(q)
		b.logsFilter.ParseErr = err
		if err == nil {
			b.logsFilter.LastGood = expr
		}
	}
}

// clearActiveFilter clears the active view's filter state (Esc inside the s bar).
func (b *BaseModel) clearActiveFilter() {
	switch b.viewMode {
	case ViewModeRequests, ViewModeRequestDetail:
		b.requestsFilter = requestsFilterState{}
	default:
		b.logsFilter = logsFilterState{}
	}
}

// clearAllFilters clears both views' s-bar filters (normal-mode Esc).
func (b *BaseModel) clearAllFilters() {
	b.logsFilter = logsFilterState{}
	b.requestsFilter = requestsFilterState{}
}

// activeFilterRaw returns the active view's RawQuery (for status / seeding).
func (b *BaseModel) activeFilterRaw() string {
	if b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail {
		return b.requestsFilter.RawQuery
	}
	return b.logsFilter.RawQuery
}

// activeFilterParseErr returns the active view's ParseErr.
func (b *BaseModel) activeFilterParseErr() error {
	if b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail {
		return b.requestsFilter.ParseErr
	}
	return b.logsFilter.ParseErr
}

// setLogsFilterQuery is a test/helper path that applies a logs filter as if the
// s bar had accepted q (updates RawQuery + LastGood, clears ParseErr on success).
func (b *BaseModel) setLogsFilterQuery(q string) {
	b.logsFilter.RawQuery = q
	expr, err := ParseLogsFilter(q)
	b.logsFilter.ParseErr = err
	if err == nil {
		b.logsFilter.LastGood = expr
	}
}

// setRequestsFilterQuery is the requests-view counterpart of setLogsFilterQuery.
func (b *BaseModel) setRequestsFilterQuery(q string) {
	b.requestsFilter.RawQuery = q
	expr, err := ParseRequestsFilter(q)
	b.requestsFilter.ParseErr = err
	if err == nil {
		b.requestsFilter.LastGood = expr
	}
}

// handleHelpKey handles keys in help mode. ModeHelp captures ALL keys: scroll
// keys scroll; esc/?/q/enter close; anything else is swallowed (plan 022 WS4).
// Intentional divergence from strix (dismiss-on-any-key): ours stays scrollable.
func (b *BaseModel) handleHelpKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "?", "q", "enter":
		b.exitHelp()
		return true
	case "j", "down":
		b.helpOffset++
	case "k", "up":
		b.helpOffset--
	case "pgdown":
		b.helpOffset += b.helpPageStep()
	case "pgup":
		b.helpOffset -= b.helpPageStep()
	case "g", "home":
		b.helpOffset = 0
	case "G", "end":
		b.helpOffset = b.helpMaxOffset()
	}
	b.clampHelpOffset()
	return true
}

// cycleTheme advances the active theme via the same path as the Theme menu
// (WS5 — one mutate+persist pair, no behavior duplication).
func (b *BaseModel) cycleTheme() tea.Cmd {
	return b.setThemeByName(nextThemeName(CurrentThemeName()))
}

// setThemeByName resolves name, installs the theme, persists, flashes, and
// re-renders. Canonical name and ResolveTheme warnings are surfaced in the
// setStatusFlash shows msg in the status bar and returns the clear command
// tagged with the NEW flash generation (StatusFlashClearMsg.Seq — stale
// timers from earlier flashes must not clear this one; CodeRabbit PR #102).
func (b *BaseModel) setStatusFlash(msg string, delay time.Duration) tea.Cmd {
	b.statusFlash = msg
	b.statusFlashSeq++
	seq := b.statusFlashSeq
	return tea.Tick(delay, func(t time.Time) tea.Msg {
		return StatusFlashClearMsg{Seq: seq}
	})
}

// status flash (WS2/WS5).
func (b *BaseModel) setThemeByName(name string) tea.Cmd {
	canonical, warnings := SetThemeByName(name)
	b.settings.Theme = canonical

	var msg string
	if err := SaveSettings(b.settings); err != nil {
		msg = "settings not saved: " + err.Error()
	} else {
		msg = themeFlashMessage(canonical, warnings)
	}
	applyTextInputTheme(&b.textInput)
	b.updateViewport()
	return b.setStatusFlash(msg, statusFlashClearDelay)
}

func themeFlashMessage(canonical string, warnings []string) string {
	msg := "theme: " + canonical
	if len(warnings) > 0 {
		msg += " — " + warnings[0]
	}
	return msg
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
		return b.setStatusFlash("settings not saved: "+err.Error(), statusFlashClearDelay)
	}
	return nil
}

// toggleTimestamps flips settings.Timestamps, persists, and re-renders (log
// lines are cached styled strings — plan 021 WS4).
func (b *BaseModel) toggleTimestamps() tea.Cmd {
	b.settings.Timestamps = !b.settings.Timestamps
	var cmd tea.Cmd
	if err := SaveSettings(b.settings); err != nil {
		cmd = b.setStatusFlash("settings not saved: "+err.Error(), statusFlashClearDelay)
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
		cmd = b.setStatusFlash("settings not saved: "+err.Error(), statusFlashClearDelay)
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
		if b.settings.MenuBar {
			b.openMenuFirst(MenuView)
		}
		return true, nil

	case "f":
		// Open the Filter menu when the bar is visible (WS8 / panel S3). Consumed
		// no-op when hidden; textinput modes never reach here (Codex #4).
		if b.settings.MenuBar {
			b.openMenuFirst(MenuFilter)
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
		// Text modes never reach here (consume ? as text). Opens help from
		// Normal only; menu-open ? is special-cased in handleMenuKey (WS4).
		b.enterHelp()
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
			// Seed from the active view's RawQuery so edits resume mid-query
			// (plan 021 WS6 / Codex #3).
			b.textInput.SetValue(b.activeFilterRaw())
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
		// Clear both views' filters and both views' search queries (D13/D8,
		// Codex #3). Resetting the logs cursor to the no-cursor sentinel makes
		// the next `/` seed its origin from the viewport again rather than the
		// stale prior match.
		b.soloProcess = ""
		b.clearAllFilters()
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
		// A wrapped entry taller than the viewport must anchor at its START
		// (the match/cursor row) — aligning the tail would scroll the cursor
		// itself off-screen (CodeRabbit PR #102).
		if sp.Last-sp.First+1 >= b.viewport.Height {
			b.viewport.SetYOffset(sp.First)
		} else {
			b.viewport.SetYOffset(sp.Last - b.viewport.Height + 1)
		}
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
				// ansi.Wrap, not Wordwrap: an unbroken token longer than the
				// width must be SPLIT — Wordwrap leaves it over-wide, the
				// terminal hard-wraps it visually, and the entry↔display-row
				// span mapping below drifts by one row (CodeRabbit PR #102).
				wrapped := ansi.Wrap(line, wrapWidth, "")
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

	// Header: bold method + full URL (curl-ish first line, plan 021 C9).
	lines = append(lines, s.Header.Render(fmt.Sprintf("Request: %s", d.ID)))
	lines = append(lines, "")
	lines = append(lines, s.Bold.Render(d.Method)+" "+d.URL)
	lines = append(lines, fmt.Sprintf("  Time:     %s", d.Timestamp))
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

	// Request headers (key Dim, value default — C9)
	if len(d.RequestHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, s.Header.Render("Request Headers"))
		lines = append(lines, formatHeaderTable(d.RequestHeaders)...)
	}

	// Response headers
	if len(d.ResponseHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, s.Header.Render("Response Headers"))
		lines = append(lines, formatHeaderTable(d.ResponseHeaders)...)
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

// formatHeaderTable renders headers as an aligned key/value table with Dim
// keys (plan 021 C9). Keys are sorted for stable output.
func formatHeaderTable(headers map[string][]string) []string {
	keys := make([]string, 0, len(headers))
	maxKey := 0
	for name := range headers {
		keys = append(keys, name)
		if len(name) > maxKey {
			maxKey = len(name)
		}
	}
	sort.Strings(keys)
	var lines []string
	for _, name := range keys {
		for _, value := range headers[name] {
			padded := name + strings.Repeat(" ", maxKey-len(name))
			lines = append(lines, fmt.Sprintf("  %s  %s", s.Dim.Render(padded), value))
		}
	}
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
	prettyJSON := false
	if shouldPrettyPrintJSON(body) {
		var buf bytes.Buffer
		if err := json.Indent(&buf, []byte(body.Data), "", "  "); err == nil {
			text = buf.String()
			prettyJSON = true
		}
	}

	var lines []string
	for _, line := range strings.Split(text, "\n") {
		// Sanitize before highlighting so ESC from the JSON highlighter is
		// not mistaken for a body control byte (C9).
		safe := sanitizeControlChars(line)
		if prettyJSON {
			safe = highlightJSONText(safe)
		}
		lines = append(lines, "  "+safe)
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

// filteredEntries returns log entries after applying filters. The s-bar query
// is evaluated via logsFilter.LastGood (plan 021 WS6); process inclusion is
// expressed with proc:/-proc: clauses (WS8 retired ModeFilter/filterProcesses).
func (b *BaseModel) filteredEntries() []domain.LogEntry {
	var result []domain.LogEntry
	expr := b.logsFilter.LastGood
	useExpr := !expr.IsEmpty()

	for _, entry := range b.logEntries {
		// Process filter
		if b.soloProcess != "" && entry.Process != b.soloProcess {
			continue
		}

		if useExpr {
			meta := b.logMeta[entry.DisplaySeq] // zero value (no level) when missing
			if !expr.Match(entry, meta) {
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
// bulk of that cost. The s-bar query uses requestsFilter.LastGood (WS6).
func (b *BaseModel) filteredProxyRequests() []proxy.RequestRecord {
	result := make([]proxy.RequestRecord, 0, len(b.proxyRequests))
	expr := b.requestsFilter.LastGood
	useExpr := !expr.IsEmpty()

	for _, req := range b.proxyRequests {
		if useExpr && !expr.Match(req) {
			continue
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

	// Format subdomain with padding; truncate over-long names so columns
	// don't drift (CodeRabbit PR #102).
	subdomain := ansi.Truncate(fmt.Sprintf("%-10s", req.Subdomain), 10, "")

	// Method token coloured by verb (C9); pad inside the styled segment so
	// ANSI resets don't sit between the verb and its column padding.
	method := httpMethodStyle(req.Method).Render(fmt.Sprintf("%-7s", req.Method))

	// Status token by class (C9): 2xx→Status2xx, 3xx→FG (unstyled), 4xx/5xx
	// themed, 0/in-flight→Dim. 1xx also Dim.
	statusTok := fmt.Sprintf("%3d", req.StatusCode)
	switch {
	case req.InFlight || req.StatusCode < 200:
		statusTok = s.Dim.Render(statusTok)
	case req.StatusCode >= 500:
		statusTok = s.Status5xx.Render(statusTok)
	case req.StatusCode >= 400:
		statusTok = s.Status4xx.Render(statusTok)
	case req.StatusCode >= 300:
		// 3xx: default FG (plan/brief) — leave unstyled.
	case req.StatusCode >= 200:
		statusTok = s.Status2xx.Render(statusTok)
	}

	// Duration column (C9): colour scale from plan WS9 — <100ms OK,
	// 100–499 default FG, 500–1999 Warn, ≥2000 Err. In-flight/stale keep
	// their glyphs (…ms / stale?) but Dim-styled (brief's "pulse" is
	// style-only; the …ms glyph is kept over a ● so row shape stays).
	durationMs := req.Duration.Milliseconds()
	var duration string
	switch {
	case b.requestIsStale(req):
		duration = s.Dim.Render("stale?")
	case req.InFlight:
		duration = s.Dim.Render("  ...ms")
	case durationMs > 9999:
		duration = s.HTTPError.Render("9999+ms")
	case durationMs >= 2000:
		duration = s.HTTPError.Render(fmt.Sprintf("%5dms", durationMs))
	case durationMs >= 500:
		duration = s.Warn.Render(fmt.Sprintf("%5dms", durationMs))
	case durationMs >= 100:
		duration = fmt.Sprintf("%5dms", durationMs) // default FG
	default:
		duration = s.HTTPSuccess.Render(fmt.Sprintf("%5dms", durationMs))
	}

	return fmt.Sprintf("%s  %s  %s %s %s  %s",
		s.Dim.Render(ts),
		s.Dim.Render(subdomain),
		method,
		statusTok,
		duration,
		req.URL,
	)
}

// httpMethodStyle returns the C9 method colour; unknown verbs stay unstyled FG.
func httpMethodStyle(method string) lipgloss.Style {
	switch strings.ToUpper(method) {
	case "GET":
		return s.HTTPGet
	case "POST":
		return s.HTTPPost
	case "PUT":
		return s.HTTPPut
	case "DELETE":
		return s.HTTPDelete
	case "PATCH":
		return s.HTTPPatch
	default:
		return lipgloss.NewStyle()
	}
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

	// Format process name with padding; truncate over-long names so columns
	// don't drift (CodeRabbit PR #102).
	procName := ansi.Truncate(fmt.Sprintf("%-10s", entry.Process), 10, "")

	// Build line
	prefix := procStyle.Render(procName)

	// Stream indicator
	streamIndicator := ""
	if entry.Stream == domain.StreamStderr {
		streamIndicator = s.Err.Render(" ERR ")
	}

	content := b.formatLogContent(entry)
	// Timestamps toggle (plan 021 WS4): omitting the fixed-width `15:04:05 `
	// prefix shifts the process-name column left — intentional, no padding
	// compensation. Default true preserves today's always-on rendering.
	if b.settings.Timestamps {
		ts := s.Dim.Render(entry.Timestamp.Format("15:04:05"))
		return fmt.Sprintf("%s %s%s %s", ts, prefix, streamIndicator, content)
	}
	return fmt.Sprintf("%s%s %s", prefix, streamIndicator, content)
}

// formatLogContent builds the log-line body (after ts/process/ERR prefix):
// JSON lines become a path=value summary (C9); level-tinted content composes
// with / search highlight. Undetected plain lines stay byte-identical to C8.
func (b *BaseModel) formatLogContent(entry domain.LogEntry) string {
	meta := b.logMeta[entry.DisplaySeq]
	line := entry.Line

	if meta.isJSON {
		// Width budget for no-wrap truncation: viewport minus the chrome
		// prefix this entry will carry. Wrap-on leaves overflow to Wordwrap.
		width := 0
		if !b.settings.Wrap && b.viewport.Width > 0 {
			width = b.logContentWidth(entry)
		}
		if summary, ok := summarizeJSONLog(line, width); ok {
			line = summary
		}
	}

	levelStyle, tint := logLevelStyle(meta)
	return b.styleLogContent(line, levelStyle, tint)
}

// logContentWidth is the columns available for the log body once the
// timestamp / process / ERR prefix are accounted for (ANSI-aware).
func (b *BaseModel) logContentWidth(entry domain.LogEntry) int {
	w := b.viewport.Width
	if w <= 0 {
		return 0
	}
	// Mirror formatLogEntry's prefix geometry with unstyled stand-ins so
	// StringWidth matches the visible columns (styled prefix has same width).
	used := 0
	if b.settings.Timestamps {
		used += len("15:04:05") + 1 // ts + space
	}
	used += 10 // process column
	if entry.Stream == domain.StreamStderr {
		used += len(" ERR ")
	}
	used += 1 // space before content
	remain := w - used
	if remain < 0 {
		return 0
	}
	return remain
}

// logLevelStyle picks the C9 content tint for a cached meta. Info uses
// LogInfo; trace stays default FG (no tint) so only debug is dim among the
// low-severity levels. Unknown / !hasLevel → no tint.
func logLevelStyle(meta logMeta) (lipgloss.Style, bool) {
	if !meta.hasLevel {
		return lipgloss.Style{}, false
	}
	switch meta.level {
	case LogLevelError:
		return s.LogError, true
	case LogLevelWarn:
		return s.LogWarn, true
	case LogLevelDebug:
		return s.LogDebug, true
	case LogLevelInfo:
		return s.LogInfo, true
	default: // trace / unknown
		return lipgloss.Style{}, false
	}
}

// styleLogContent applies level tint then / search highlight. Highlight is
// resolved on the PLAIN content so byte offsets stay valid; level colour wraps
// the non-match spans so SearchHighlight's reset does not leak the level tint
// past the match (C9). When no tint, this is highlightLogLine alone.
func (b *BaseModel) styleLogContent(line string, levelStyle lipgloss.Style, tint bool) string {
	if !tint {
		return b.highlightLogLine(line)
	}
	q := b.logSearchQuery
	if q == "" || !isASCIINoESC(q) || !isASCIINoESC(line) {
		return levelStyle.Render(line)
	}
	idx := strings.Index(strings.ToLower(line), strings.ToLower(q))
	if idx < 0 {
		return levelStyle.Render(line)
	}
	end := idx + len(q)
	return levelStyle.Render(line[:idx]) +
		s.SearchHighlight.Render(line[idx:end]) +
		levelStyle.Render(line[end:])
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
	rowY, panelRowOK := b.processPanelRowY()
	const chipSep = "  "
	chipSepW := ansi.StringWidth(chipSep)
	chipCol := 1 // Header Padding(0,1) left gutter

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
			segment := style.Render(key+name) + dot
			items = append(items, segment)
			if panelRowOK {
				w := ansi.StringWidth(segment)
				hits := b.ensureHits()
				hits.chips = append(hits.chips, processChipHit{
					Index: i,
					Rect:  HitRect{X: chipCol, Y: rowY, W: w, H: 1},
				})
				chipCol += w + chipSepW
			}
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
// with the visible/total count. Precedence differs by view (D13 / Codex #3):
//   - Requests view: the `/` search indicator wins when a query is set —
//     "/<query> (i/k)" with i the cursor's 1-based position among matches when
//     the cursor is on a match, else "/<query> (k matches)" (0 included) — with
//     "| filter: <raw>" appended when the `s` filter is also active. soloProcess
//     is a logs-view concept and is never shown here.
//   - Logs view: same search-indicator shape, then solo, then the `s` filter.
//     Both views now show the raw filter query when set (Codex #3 unified the
//     prior asymmetry where requests appended it and logs hid it). An invalid
//     mid-edit query keeps LastGood filtering and adds an "invalid filter" hint.
//   - Otherwise (either view): the `s` filter line, then the default hint.
func (b *BaseModel) statusLeftDefault(extraInfo string, requests []proxy.RequestRecord, entries []domain.LogEntry) string {
	filterRaw, filterInvalid := b.statusFilterBits()

	if b.viewMode == ViewModeRequests && b.requestSearchQuery != "" {
		position, total := b.requestSearchMatchInfo(requests)
		var indicator string
		if position > 0 {
			indicator = fmt.Sprintf("/%s (%d/%d)", b.requestSearchQuery, position, total)
		} else {
			indicator = fmt.Sprintf("/%s (%d matches)", b.requestSearchQuery, total)
		}
		if filterRaw != "" {
			indicator += fmt.Sprintf(" | filter: %s", filterRaw)
			if filterInvalid {
				indicator += " [invalid filter]"
			}
		}
		return indicator
	}
	if b.viewMode == ViewModeLogs && b.logSearchQuery != "" {
		position, total := b.logSearchMatchInfo(entries)
		var indicator string
		if position > 0 {
			indicator = fmt.Sprintf("/%s (%d/%d)", b.logSearchQuery, position, total)
		} else {
			indicator = fmt.Sprintf("/%s (%d matches)", b.logSearchQuery, total)
		}
		if filterRaw != "" {
			indicator += fmt.Sprintf(" | filter: %s", filterRaw)
			if filterInvalid {
				indicator += " [invalid filter]"
			}
		}
		return indicator
	}
	if b.viewMode != ViewModeRequests && b.soloProcess != "" {
		return fmt.Sprintf("Showing: %s (ESC to clear)", b.soloProcess)
	}
	if filterRaw != "" {
		msg := fmt.Sprintf("Filter: %s", filterRaw)
		if filterInvalid {
			msg += " [invalid filter]"
		}
		return msg + " (ESC to clear)"
	}
	return b.statusDefaultHint(extraInfo)
}

// statusFilterBits returns the active view's raw filter query and whether it
// currently fails to parse (LastGood still evaluates).
func (b *BaseModel) statusFilterBits() (raw string, invalid bool) {
	if b.viewMode == ViewModeRequests || b.viewMode == ViewModeRequestDetail {
		return b.requestsFilter.RawQuery, b.requestsFilter.ParseErr != nil
	}
	return b.logsFilter.RawQuery, b.logsFilter.ParseErr != nil
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
	case ModeSearch:
		left = "Search: " + b.textInput.View()
	case ModeStringFilter:
		left = "Filter: " + b.textInput.View()
		if b.activeFilterParseErr() != nil {
			left += " [invalid filter]"
		}
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
		h := b.ensureHits()
		h.chips = h.chips[:0]
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

func (b *BaseModel) helpTitle(suffix string) string {
	title := "Prox - Process Manager"
	if b.helpConfig.TitleSuffix != "" {
		title += " " + b.helpConfig.TitleSuffix
	}
	if suffix != "" {
		title += " " + suffix
	}
	return title
}

func (b *BaseModel) helpQuit() string {
	if b.helpConfig.QuitMessage != "" {
		return b.helpConfig.QuitMessage
	}
	return "Quit"
}

// logsHelpText is the full (unwindowed) logs help content.
func (b *BaseModel) logsHelpText() string {
	help := fmt.Sprintf(`%s

Navigation:
  j/k, ↑/↓     Scroll (up pauses auto-follow)
  PgUp/PgDn    Half-page scroll
  g/Home       Jump to top (pauses follow)
  G/End        Jump to bottom (resumes follow)
  F            Toggle auto-follow
  Tab          Switch to Requests view
  scroll wheel Scroll (3 lines per notch; up pauses follow)

Filter & search:
  s            Filter bar (query language, live)
               e.g. proc:api level:error -health
  f            Filter menu (process + level checks)
  /            Search — jump cursor to match (does not hide lines)
  n/N          Next/previous search match
  Esc          Clear filters, search, and solo

Processes:
  1-9          Solo a process (toggle); click panel chip too

View & chrome:
  p            Toggle process panel
  T            Toggle timestamps in log lines
  w            Toggle soft-wrap
  m            Toggle menu bar
  v            Open View menu (bar visible)
  t            Cycle theme
  ?            This help

Copy (grab-for-agent):
  y            Copy parked search line (when cursor set)

Actions:
  r            Restart soloed process
  q/Ctrl+C     %s

Mouse:
  wheel        Scroll logs; scroll open dropdown (not viewport) when menu open
  click line   Park cursor on that entry (disengages follow)
  click chip   Solo/unsolo process
  menu bar     Click or hover cells; click dropdown rows; ←/→ switch open menus
  help open    Wheel scrolls when content exceeds the modal; click outside closes`,
		b.helpTitle("[Logs View]"), b.helpQuit())

	return help
}

// requestsHelpText is the full (unwindowed) requests help content.
func (b *BaseModel) requestsHelpText() string {
	help := fmt.Sprintf(`%s

Navigation (cursor row ❯):
  j/k, ↑/↓     Move cursor (up pauses follow; onto newest resumes)
  PgUp/PgDn    Move cursor half a page
  g/Home       Cursor to top (pauses follow)
  G/End        Cursor to bottom (resumes follow)
  F            Toggle auto-follow
  Tab          Switch to Logs view
  scroll wheel Move cursor (3 rows/notch; follow rules match j/k)

Filter & search:
  s            Filter bar (query language, live)
               e.g. method:GET status:5xx host:api url:/orders
  f            Filter menu (status class, methods)
  /            Search URL/method/subdomain (navigate, not filter)
  n/N          Next/previous search match
  Esc          Back from detail, or clear filters/search

Requests:
  Enter        Open detail for cursor row
  click row    Move cursor; double-click opens detail

View & chrome:
  p/T/w/m/v/t  Same as Logs view (panel, timestamps, wrap, menu, theme)
  ?            This help

Copy (grab-for-agent):
  y            Copy full request ID (cursor row)
  c            Copy as curl
  Y            Copy detail JSON (detail view)

Actions:
  q/Ctrl+C     %s

Mouse:
  wheel        Move cursor; scroll open dropdown (not viewport) when menu open
  click row    Move cursor; double-click opens detail
  click chip   Solo/unsolo process
  menu bar     Click or hover cells; click dropdown rows; ←/→ between menus
  help open    Wheel scrolls when content exceeds the modal; click outside closes`,
		b.helpTitle("[Requests View]"), b.helpQuit())

	return help
}

// detailHelpText is the full (unwindowed) detail help content.
func (b *BaseModel) detailHelpText() string {
	help := fmt.Sprintf(`%s

Navigation:
  j/k, ↑/↓     Scroll detail content
  PgUp/PgDn    Page scroll
  scroll wheel Scroll (3 lines per notch)
  Esc          Back to requests list

Copy (grab-for-agent):
  y            Copy full request ID
  c            Copy as curl
  Y            Copy wire JSON (exact API payload)

View & chrome:
  ?            This help
  q/Ctrl+C     %s

Mouse:
  wheel        Scroll detail; scroll open dropdown when menu open
  menu bar     Click or hover cells; click dropdown rows
  help open    Wheel scrolls when content exceeds the modal; click outside closes`,
		b.helpTitle("[Request Detail]"), b.helpQuit())

	return help
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
