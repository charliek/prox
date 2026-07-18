package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
	"github.com/charliek/prox/internal/proxy"
)

// maxLogEntries is the maximum number of log entries to keep in memory
const maxLogEntries = 1000

// maxProxyRequests is the maximum number of proxy requests to keep in memory
const maxProxyRequests = 1000

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

	// Request detail view
	selectedRequestID string
	requestDetail     *RequestDetailData
	detailLoading     bool
	detailError       error

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

	return BaseModel{
		processes:       make([]domain.ProcessInfo, 0),
		logEntries:      make([]domain.LogEntry, 0),
		proxyRequests:   make([]proxy.RequestRecord, 0),
		textInput:       ti,
		mode:            ModeNormal,
		viewMode:        ViewModeLogs,
		filterProcesses: make(map[string]bool),
		followMode:      true,
		helpConfig:      helpConfig,
	}
}

// handleWindowSize handles window resize messages
func (b *BaseModel) handleWindowSize(msg tea.WindowSizeMsg) {
	b.width = msg.Width
	b.height = msg.Height

	headerHeight := 4 // Process panel
	footerHeight := 2 // Status bar
	verticalMargins := headerHeight + footerHeight

	viewportHeight := msg.Height - verticalMargins
	if viewportHeight < 1 {
		viewportHeight = 1
	}

	if !b.ready {
		b.viewport = viewport.New(msg.Width, viewportHeight)
		b.viewport.YPosition = headerHeight
		b.ready = true
	} else {
		b.viewport.Width = msg.Width
		b.viewport.Height = viewportHeight
	}
}

// handleLogEntry handles a new log entry message
func (b *BaseModel) handleLogEntry(entry domain.LogEntry) {
	// Check if we're at/near bottom BEFORE adding new content
	wasNearBottom := b.isNearBottom()

	b.logEntries = append(b.logEntries, entry)
	// Keep only last entries - create new slice to release memory from old entries
	if len(b.logEntries) > maxLogEntries {
		newEntries := make([]domain.LogEntry, maxLogEntries)
		copy(newEntries, b.logEntries[len(b.logEntries)-maxLogEntries:])
		b.logEntries = newEntries
	}
	b.updateViewport()

	// If user was at bottom, re-enable follow mode and stay at bottom
	if wasNearBottom {
		b.followMode = true
		b.viewport.GotoBottom()
	} else if b.followMode {
		b.viewport.GotoBottom()
	}
}

// handleProxyRequest handles a new proxy request message. It upserts by ID:
// a same-ID re-record (in-flight followed by its completion) replaces the
// existing row in place rather than appending a duplicate, scanning from the
// newest entry since that's where a live in-flight row lives. In-place
// replacement keeps every other row's index stable, and the ID-anchored cursor
// (resolveRequestCursor) rides along on its row regardless.
func (b *BaseModel) handleProxyRequest(req proxy.RequestRecord) {
	replaced := false
	if req.ID != "" {
		for i := len(b.proxyRequests) - 1; i >= 0; i-- {
			if b.proxyRequests[i].ID == req.ID {
				b.proxyRequests[i] = req
				replaced = true
				break
			}
		}
	}
	if !replaced {
		b.proxyRequests = append(b.proxyRequests, req)
		// Keep only last requests - create new slice to release memory from old requests
		if len(b.proxyRequests) > maxProxyRequests {
			newRequests := make([]proxy.RequestRecord, maxProxyRequests)
			copy(newRequests, b.proxyRequests[len(b.proxyRequests)-maxProxyRequests:])
			b.proxyRequests = newRequests
		}
	}

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
		// Logs view: `/` is a substring filter committed on Enter (D14).
		b.searchPattern = b.textInput.Value()
		b.updateViewport()
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

// handleNavigationKey handles common navigation keys
// Returns true if the key was handled
func (b *BaseModel) handleNavigationKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "tab":
		// Toggle between Logs and Requests views (only if not in detail view)
		switch b.viewMode {
		case ViewModeLogs:
			b.viewMode = ViewModeRequests
		case ViewModeRequests:
			b.viewMode = ViewModeLogs
		}
		// In detail view, tab does nothing
		b.updateViewport()
		return true

	case "?":
		b.mode = ModeHelp
		return true

	case "f":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeFilter
			b.textInput.Focus()
		}
		return true

	case "/":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeSearch
			b.textInput.SetValue("")
			b.textInput.Focus()
		}
		return true

	case "s":
		if b.viewMode != ViewModeRequestDetail {
			b.mode = ModeStringFilter
			b.textInput.SetValue("")
			b.textInput.Focus()
		}
		return true

	case "n", "N":
		// Requests-view search navigation only: n jumps to the next match
		// strictly after the cursor, N to the previous, both wrapping (D13).
		// No-op (and unhandled elsewhere) outside the requests view.
		if b.viewMode == ViewModeRequests {
			dir := 1
			if msg.String() == "N" {
				dir = -1
			}
			b.cycleRequestSearchMatch(dir)
			b.updateViewport()
			return true
		}
		return false

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
		return true

	case "esc":
		// In detail view, go back to requests list
		if b.viewMode == ViewModeRequestDetail {
			b.viewMode = ViewModeRequests
			b.selectedRequestID = ""
			b.requestDetail = nil
			b.detailError = nil
			b.updateViewport()
			return true
		}
		// Clear filters (and the requests-view search query — D13).
		b.soloProcess = ""
		b.searchPattern = ""
		b.requestSearchQuery = ""
		b.updateViewport()
		return true

	case "up", "k":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(-1)
			return true
		}
		b.viewport.LineUp(1)
		b.followMode = false
		return true

	case "down", "j":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(1)
			return true
		}
		b.viewport.LineDown(1)
		return true

	case "pgup":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(-b.halfPageStep())
			return true
		}
		b.viewport.HalfViewUp()
		b.followMode = false
		return true

	case "pgdown":
		if b.viewMode == ViewModeRequests {
			b.moveRequestCursor(b.halfPageStep())
			return true
		}
		b.viewport.HalfViewDown()
		return true

	case "home", "g":
		if b.viewMode == ViewModeRequests {
			requests := b.filteredProxyRequests()
			b.setRequestCursor(requests, 0)
			b.followMode = false
			b.updateViewport()
			return true
		}
		b.viewport.GotoTop()
		b.followMode = false
		return true

	case "end", "G":
		if b.viewMode == ViewModeRequests {
			requests := b.filteredProxyRequests()
			b.followMode = true
			b.setRequestCursor(requests, len(requests)-1)
			b.updateViewport()
			return true
		}
		b.viewport.GotoBottom()
		b.followMode = true
		return true

	case "F":
		if b.viewMode == ViewModeRequests {
			b.followMode = !b.followMode
			if b.followMode {
				// Toggling follow on pins the cursor to the newest row.
				requests := b.filteredProxyRequests()
				b.setRequestCursor(requests, len(requests)-1)
			}
			b.updateViewport()
			return true
		}
		b.followMode = !b.followMode
		if b.followMode {
			b.viewport.GotoBottom()
		}
		return true
	}

	return false
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

// updateViewport updates the viewport content
func (b *BaseModel) updateViewport() {
	var lines []string

	switch b.viewMode {
	case ViewModeRequestDetail:
		lines = b.formatRequestDetail()
	case ViewModeRequests:
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
				marker = cursorStyle.Render("❯ ")
			}
			lines = append(lines, marker+b.formatProxyRequest(req))
		}
	default: // ViewModeLogs
		entries := b.filteredEntries()
		for _, entry := range entries {
			line := b.formatLogEntry(entry)
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	b.viewport.SetContent(content)

	// Cursor visibility invariant for the requests view (D7). Runs after
	// SetContent so the marker is on-screen after every transition, not just
	// keypresses. Follow mode overrides to the bottom. Logs/detail views keep
	// their own scroll untouched.
	if b.viewMode == ViewModeRequests {
		if b.followMode {
			b.viewport.GotoBottom()
		} else {
			b.ensureCursorVisible()
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
		lines = append(lines, errorStyle.Render("Error: "+b.detailError.Error()))
		return lines
	}

	if b.requestDetail == nil {
		lines = append(lines, "No request selected")
		return lines
	}

	d := b.requestDetail

	// Header info
	lines = append(lines, headerStyle.Render(fmt.Sprintf("Request: %s", d.ID)))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  Time:     %s", d.Timestamp))
	lines = append(lines, fmt.Sprintf("  Method:   %s", d.Method))
	lines = append(lines, fmt.Sprintf("  URL:      %s", d.URL))
	lines = append(lines, fmt.Sprintf("  Status:   %d", d.StatusCode))
	if d.InFlight {
		lines = append(lines, "  Duration: (in flight)")
	} else {
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
		lines = append(lines, dimStyle.Render("(request in flight — details arrive on completion)"))
	}

	// Request headers
	if len(d.RequestHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, headerStyle.Render("Request Headers"))
		for name, values := range d.RequestHeaders {
			for _, value := range values {
				lines = append(lines, fmt.Sprintf("  %s: %s", dimStyle.Render(name), value))
			}
		}
	}

	// Response headers
	if len(d.ResponseHeaders) > 0 {
		lines = append(lines, "")
		lines = append(lines, headerStyle.Render("Response Headers"))
		for name, values := range d.ResponseHeaders {
			for _, value := range values {
				lines = append(lines, fmt.Sprintf("  %s: %s", dimStyle.Render(name), value))
			}
		}
	}

	// Request body
	lines = append(lines, renderBodySection("Request Body", d.RequestBody)...)

	// Response body
	lines = append(lines, renderBodySection("Response Body", d.ResponseBody)...)

	// Footer hint
	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Press ESC to go back"))

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
	lines := []string{"", headerStyle.Render(bodySectionTitle(title, b))}
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
// unavailable (evicted) notice, a binary-data marker, or the body text split
// into lines. Non-binary JSON bodies (Content-Type contains "json", or the
// raw text is itself valid JSON) are pretty-printed 2-space indented; any
// json.Indent failure falls back to the raw text. Text otherwise renders
// unchanged except that ASCII control characters (< 0x20, other than tab and
// the newlines used for line splitting) and DEL (0x7F) are replaced with the
// Unicode replacement character, so ESC/BEL/OSC sequences from a captured body
// cannot manipulate the terminal. (Classification usually marks such bodies
// binary, but a socket-supplied record could lie; this is a cheap defense.)
func renderBodyLines(body *BodyData) []string {
	if body.Unavailable {
		return []string{dimStyle.Render("(body no longer available)")}
	}
	if body.IsBinary {
		return []string{dimStyle.Render("[binary data]")}
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

// filteredProxyRequests returns proxy requests after applying filters
func (b *BaseModel) filteredProxyRequests() []proxy.RequestRecord {
	var result []proxy.RequestRecord

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
	statusStyle := httpSuccessStyle
	switch {
	case req.StatusCode < 100:
		statusStyle = dimStyle // Gray for unknown/error (status 0)
	case req.StatusCode < 200:
		statusStyle = dimStyle // Gray for informational 1xx
	case req.StatusCode >= 500:
		statusStyle = httpErrorStyle
	case req.StatusCode >= 400:
		statusStyle = httpWarningStyle
	case req.StatusCode >= 300:
		statusStyle = httpRedirectStyle
	}
	status := statusStyle.Render(fmt.Sprintf("%3d", req.StatusCode))

	// Format duration with overflow handling. In-flight rows have no
	// duration yet (the response is still streaming) — render dots in place
	// of digits, padded to the same 5-char width so columns stay aligned.
	durationMs := req.Duration.Milliseconds()
	var duration string
	switch {
	case req.InFlight:
		duration = "  ..."
	case durationMs > 9999:
		duration = "9999+"
	default:
		duration = fmt.Sprintf("%5d", durationMs)
	}

	return fmt.Sprintf("%s  %s  %s %s %sms  %s",
		dimStyle.Render(ts),
		dimStyle.Render(subdomain),
		method,
		status,
		dimStyle.Render(duration),
		req.URL,
	)
}

// formatLogEntry formats a single log entry for display
func (b *BaseModel) formatLogEntry(entry domain.LogEntry) string {
	// Get process color
	procStyle := getProcessStyle(entry.Process, b.processes)

	// Format timestamp
	ts := entry.Timestamp.Format("15:04:05")

	// Format process name with padding
	procName := fmt.Sprintf("%-10s", entry.Process)

	// Build line
	prefix := procStyle.Render(procName)
	timestamp := dimStyle.Render(ts)

	// Stream indicator
	streamIndicator := ""
	if entry.Stream == domain.StreamStderr {
		streamIndicator = errorStyle.Render(" ERR ")
	}

	return fmt.Sprintf("%s %s%s %s", timestamp, prefix, streamIndicator, entry.Line)
}

// processPanel renders the process status header
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

		// Show number key (only in logs view where 1-9 keys work)
		if b.viewMode == ViewModeLogs {
			key := fmt.Sprintf("%d:", i+1)
			items = append(items, style.Render(key+name))
		} else {
			items = append(items, style.Render(name))
		}
	}

	header := lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(items, "  "))
	return headerStyle.Render(header)
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
//   - Logs view: today's precedence — solo wins, then the `s` filter.
//   - Otherwise (either view): the `s` filter line, then the default hint.
func (b *BaseModel) statusLeftDefault(extraInfo string, requests []proxy.RequestRecord) string {
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

	// Requests-view filtered list, computed once and shared below between the
	// left-side search indicator and the right-side visible count so a single
	// render doesn't rescan b.proxyRequests twice.
	var requests []proxy.RequestRecord
	if b.viewMode == ViewModeRequests {
		requests = b.filteredProxyRequests()
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
		left = b.statusLeftDefault(extraInfo, requests)
	}

	// Right side: follow mode and count
	var visible, total int
	var label string
	if b.viewMode == ViewModeRequests {
		visible = len(requests)
		total = len(b.proxyRequests)
		label = "requests"
	} else {
		visible = len(b.filteredEntries())
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

	leftPart := statusStyle.Width(leftWidth).Render(left)
	rightPart := statusStyle.Render(right)

	return lipgloss.JoinHorizontal(lipgloss.Top, leftPart, "  ", rightPart)
}

// mainView renders the main TUI layout
func (b *BaseModel) mainView(extraStatusInfo string) string {
	var sb strings.Builder

	// Process panel at top
	sb.WriteString(b.processPanel())
	sb.WriteString("\n")

	// Main log viewport
	sb.WriteString(b.viewport.View())
	sb.WriteString("\n")

	// Status bar at bottom
	sb.WriteString(b.statusBar(extraStatusInfo))

	return sb.String()
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

Filtering:
  1-9        Solo process (toggle)
  f          Filter mode (process selection)
  /          Substring filter (hide non-matching, commit on Enter)
  s          Substring filter (hide non-matching, applied live)
  ESC        Clear filters

Other:
  r          Restart selected process (1-9 to select)
  ?          Toggle help
  q/Ctrl+C   %s

Press any key to close help...
`, title, quitMsg)

	return helpStyle.Render(help)
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

Navigation (moves the cursor row):
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

	return helpStyle.Render(help)
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

// convertRequestRecordToDetail converts a proxy.RequestRecord to RequestDetailData.
// This is shared between Model (local mode) and ClientModel (API mode).
// Bodies are loaded and content-decoded via the proxy loader so FilePath-backed
// (disk-spilled) bodies are read rather than dropped.
func convertRequestRecordToDetail(req proxy.RequestRecord) *RequestDetailData {
	return convertRequestRecordToDetailWithDirs(req, captureAllowedDirs())
}

// convertRequestRecordToDetailWithDirs is convertRequestRecordToDetail with an
// explicit FilePath allowlist, separated so tests can supply a temp directory.
func convertRequestRecordToDetailWithDirs(req proxy.RequestRecord, allowedDirs []string) *RequestDetailData {
	detail := &RequestDetailData{
		ID:         req.ID,
		Timestamp:  req.Timestamp.Format("2006-01-02 15:04:05.000"),
		Method:     req.Method,
		URL:        req.URL,
		Subdomain:  req.Subdomain,
		StatusCode: req.StatusCode,
		DurationMs: req.Duration.Milliseconds(),
		RemoteAddr: req.RemoteAddr,
		InFlight:   req.InFlight,
	}

	if req.Details != nil {
		detail.RequestHeaders = req.Details.RequestHeaders
		detail.ResponseHeaders = req.Details.ResponseHeaders

		if req.Details.RequestBody != nil {
			detail.RequestBody = convertCapturedBodyToBodyData(req.Details.RequestBody, allowedDirs)
		}
		if req.Details.ResponseBody != nil {
			detail.ResponseBody = convertCapturedBodyToBodyData(req.Details.ResponseBody, allowedDirs)
		}
	}

	return detail
}

// convertCapturedBodyToBodyData loads/decodes a captured body for TUI display.
// An unavailable (evicted) body is marked so the renderer can note it rather
// than showing garbage; binary and decoded-text semantics follow the loader.
func convertCapturedBodyToBodyData(body *proxy.CapturedBody, allowedDirs []string) *BodyData {
	bd := &BodyData{
		Size:            body.Size,
		Truncated:       body.Truncated,
		ContentType:     body.ContentType,
		ContentEncoding: body.ContentEncoding,
		IsBinary:        body.IsBinary,
	}

	decoded, err := proxy.LoadDecodedBody(body, allowedDirs)
	if err != nil || !decoded.Available {
		bd.Unavailable = true
		if decoded.UnavailableReason != "" {
			bd.UnavailableReason = decoded.UnavailableReason
		} else {
			bd.UnavailableReason = "evicted"
		}
		return bd
	}

	bd.IsBinary = decoded.IsBinary
	// Defense in depth (mirrors the API serve path): never string-convert bytes
	// that are not valid UTF-8, even if the loaded record claims they are text —
	// a socket-supplied flag or a mutated disk file must not reach the terminal
	// as raw control bytes.
	if !decoded.IsBinary && utf8.Valid(decoded.Data) {
		bd.Data = string(decoded.Data)
	} else {
		bd.IsBinary = true
	}
	return bd
}

// captureAllowedDirs returns the directories a captured body's FilePath may
// resolve within for the in-TUI loader: the local project capture dir
// (cwd/.prox/capture) and the shared daemon capture dir under the user's home.
func captureAllowedDirs() []string {
	var primary string
	if cwd, err := os.Getwd(); err == nil {
		primary = filepath.Join(cwd, constants.CaptureDirectory)
	}
	return proxy.CaptureAllowedDirs(primary)
}
