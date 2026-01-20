# Phase 6: TUI

## Status: COMPLETE

## Objective

Build the interactive terminal UI using bubbletea with process status, log viewing, filtering, and search.

## Tasks

### Core Model (`internal/tui/model.go`)

- [ ] Model struct with all state
- [ ] Mode enum (Normal, Filter, Search, StringFilter)
- [ ] Initialize with Supervisor and LogManager
- [ ] Viewport for log scrolling

### App Setup (`internal/tui/app.go`)

- [ ] Bubbletea program setup
- [ ] Initial commands (subscribe to logs, get processes)
- [ ] Window resize handling
- [ ] Quit handling

### Update Logic (`internal/tui/update.go`)

- [ ] Key handling per mode
- [ ] Log entry message handling
- [ ] Process update message handling
- [ ] Timer ticks for refresh

### View Rendering (`internal/tui/view.go`)

- [ ] Process panel (top)
- [ ] Log viewer (middle)
- [ ] Status bar (bottom)
- [ ] Layout calculation

### Keybindings (`internal/tui/keys.go`)

- [ ] Define all keybindings
- [ ] Help text generation

### Styles (`internal/tui/styles.go`)

- [ ] Lipgloss styles for all elements
- [ ] Process state colors
- [ ] Stream colors (stdout vs stderr)

### Components

- [ ] `components/process_panel.go` - Process status display
- [ ] `components/log_viewer.go` - Scrollable log view
- [ ] `components/filter_modal.go` - Process filter modal
- [ ] `components/help_overlay.go` - Help modal

## Layout

```
┌─ processes ──────────────────────────────────────────────┐
│ [1] ● web     running   [2] ● api    running             │
│ [3] ● worker  starting  [4] ○ cron   stopped             │
├─ logs (showing: all) ────────────────────────────────────┤
│ 10:32:01 web    │ GET /api/users 200 12ms                │
│ 10:32:01 api    │ connected to database                  │
│ 10:32:02 worker │ processing job 123                     │
├──────────────────────────────────────────────────────────┤
│ [f]ilter [/]search [s]tring filter [r]estart [?]help [q] │
└──────────────────────────────────────────────────────────┘
```

## Keybindings

| Key | Action |
|-----|--------|
| ↑/↓/j/k | Scroll logs |
| PgUp/PgDn | Scroll page |
| Home/End/g/G | Jump to start/end |
| 1-9 | Solo process (press again for all) |
| f | Open process filter modal |
| / | Search mode (highlight) |
| n/N | Next/previous match |
| s | String filter mode (hide non-matching) |
| Esc | Clear filter/search, exit mode |
| r | Restart highlighted process |
| ? | Help overlay |
| q | Quit |

## Modes

### Normal Mode
- Scroll logs
- Quick keys active
- Status bar shows hints

### Filter Mode (f)
```
┌─ filter processes ───────────────────┐
│ [x] web                              │
│ [x] api                              │
│ [ ] worker                           │
├──────────────────────────────────────┤
│ [space] toggle  [a]ll  [n]one        │
│ [enter] apply   [esc] cancel         │
└──────────────────────────────────────┘
```

### Search Mode (/)
- Input at bottom
- Matches highlighted
- n/N navigation
- Enter/Esc exits

### String Filter Mode (s)
- Input at bottom
- Only matching lines shown
- Header shows filter: `logs (filter: "ERROR")`
- Esc clears

## Messages

```go
type LogEntryMsg LogEntry
type ProcessUpdateMsg []ProcessInfo
type TickMsg time.Time
type FilterAppliedMsg LogFilter
```

## Verification

```bash
go test ./internal/tui/... -v

# Manual verification
./prox up --tui
# Test all keybindings
# Test scrolling
# Test filter modal
# Test search
# Test string filter
# Test resize
```

## Deviations & Notes

| Item | Description |
|------|-------------|
| Simplified components | Combined components into single files instead of separate component files |
| n/N navigation | Not implemented - search matches are tracked but no navigation between them |
| Filter modal | Simplified to use keyboard shortcuts rather than full modal |
| Process restart | TODO placeholder - needs process selection implementation |

## Completion Checklist

- [x] TUI launches with --tui flag
- [x] Process panel shows all processes with status
- [x] Log viewer scrolls correctly
- [x] 1-9 solo process filtering works
- [x] f enters filter mode
- [x] / enters search mode
- [ ] n/N navigate matches (not implemented)
- [x] s enters string filter mode (live filtering)
- [x] Esc clears filters/modes
- [ ] r restarts selected process (TODO placeholder)
- [x] ? shows help overlay
- [x] q quits cleanly
- [x] Window resize works
- [x] Colors render correctly
- [x] Tests cover core functionality
