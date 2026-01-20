# TUI Reference

The interactive terminal UI provides real-time log viewing with filtering and search.

## Starting the TUI

```bash
prox up --tui
```

Or start specific processes:

```bash
prox up --tui web api
```

## Layout

```
┌─ processes ──────────────────────────────────────────────┐
│ [1] ● web     running   [2] ● api    running             │
│ [3] ● worker  starting  [4] ○ cron   stopped             │
├─ logs (showing: all) ────────────────────────────────────┤
│ 10:32:01 web    │ GET /api/users 200 12ms                │
│ 10:32:01 api    │ connected to database                  │
│ 10:32:02 worker │ processing job 123                     │
│ 10:32:02 web    │ GET /api/posts 200 8ms                 │
│ 10:32:03 api    │ WARN: connection pool running low      │
│ ...                                                      │
├──────────────────────────────────────────────────────────┤
│ [f]ilter [/]search [s]tring filter [r]estart [?]help [q] │
└──────────────────────────────────────────────────────────┘
```

## Keybindings

| Key | Action |
|-----|--------|
| `↑` / `↓` / `j` / `k` | Scroll logs |
| `PgUp` / `PgDn` | Scroll page |
| `scroll wheel` | Scroll logs |
| `Home` / `End` / `g` / `G` | Jump to start/end |
| `1-9` | Solo process (press again for all) |
| `f` | Open process filter (multi-select) |
| `/` | Search (highlight matches) |
| `n` / `N` | Next/previous search match |
| `s` | String filter (hide non-matching) |
| `Esc` | Clear filter/search, exit mode |
| `r` | Restart highlighted process |
| `?` | Show help overlay |
| `q` | Quit |

## Process Filter Mode

Press `f` to open the multi-select process filter:

```
┌─ filter processes ───────────────────┐
│ [x] web                              │
│ [x] api                              │
│ [ ] worker                           │
│ [x] cron                             │
├──────────────────────────────────────┤
│ [space] toggle  [a]ll  [n]one        │
│ [enter] apply   [esc] cancel         │
└──────────────────────────────────────┘
```

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate list |
| `Space` | Toggle selection |
| `a` | Select all |
| `n` | Select none |
| `Enter` | Apply filter |
| `Esc` | Cancel |

## Search Mode

Press `/` to enter search mode:

- Input field appears at bottom
- Matching text is highlighted in the log view
- Logs continue to flow while searching
- `n` / `N` navigate between matches
- `Enter` exits search mode (highlights remain)
- `Esc` clears search and highlights

## String Filter Mode

Press `s` to enter string filter mode:

- Input field appears at bottom
- Only lines matching the filter are shown
- Non-matching lines are hidden
- Header shows active filter: `logs (filter: "ERROR")`
- `Esc` clears the filter

## Help Overlay

Press `?` to show all keybindings in a modal overlay. Press any key to dismiss.
