# TUI Reference

The interactive terminal UI provides real-time log viewing with filtering and search, plus a proxy request viewer when the proxy is enabled.

## Starting the TUI

```bash
prox up --tui
```

Or start specific processes:

```bash
prox up --tui web api
```

## Views

The TUI has two views you can switch between with `Tab`:

- **Logs View** - Real-time process logs with filtering
- **Requests View** - Real-time HTTP proxy requests (when proxy is enabled)

## Logs View Layout

```text
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
│ Tab: switch view | ? for help  [Logs] [FOLLOW] 45 lines  │
└──────────────────────────────────────────────────────────┘
```

## Requests View Layout

```text
┌─ processes ──────────────────────────────────────────────┐
│ ● web     running   ● api    running                     │
├─ requests ───────────────────────────────────────────────┤
│ 15:04:05  api        GET  200   45ms  /api/v1/users      │
│ 15:04:05  app        POST 201  120ms  /api/v1/posts      │
│ 15:04:06  api        GET  404   12ms  /api/v1/missing    │
│ 15:04:07  web        GET  200   23ms  /assets/main.js    │
│ ...                                                      │
├──────────────────────────────────────────────────────────┤
│ Tab: switch view | ? for help  [Requests] [FOLLOW] 12    │
└──────────────────────────────────────────────────────────┘
```

Status codes are color-coded: green (2xx), cyan (3xx), yellow (4xx), red (5xx), gray (0/unknown).

## Keybindings

### General

| Key | Action |
| --- | ------ |
| `Tab` | Switch between Logs and Requests views |
| `↑` / `↓` / `j` / `k` | Scroll |
| `PgUp` / `PgDn` | Scroll page |
| `scroll wheel` | Scroll |
| `Home` / `End` / `g` / `G` | Jump to start/end |
| `F` | Toggle auto-follow mode |
| `Esc` | Clear filter/search, exit mode |
| `?` | Show help overlay |
| `q` | Quit |

### Logs View

| Key | Action |
| --- | ------ |
| `1-9` | Solo process (press again for all) |
| `f` | Open process filter (multi-select) |
| `/` | Search (highlight matches) |
| `n` / `N` | Next/previous search match |
| `s` | String filter (hide non-matching) |
| `r` | Restart highlighted process |

### Requests View

| Key | Action |
| --- | ------ |
| `s` | String filter (on URL/method/subdomain) |

## Process Filter Mode

Press `f` to open the multi-select process filter:

```text
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
| --- | ------ |
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
