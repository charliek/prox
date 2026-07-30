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

## Process State Colors

The processes panel color-codes each process name by its state, including the states introduced by [dependencies and tasks](../guides/dependencies.md):

| State | Color |
| ----- | ----- |
| `running` | green |
| `stopped` | gray |
| `crashed` | red |
| `starting` | yellow |
| `stopping` | yellow |
| `waiting` | amber (distinct from the starting yellow) |
| `blocked` | red, bold (a launch failure, same family as crashed) |
| `completed` | gray (a finished task rests like a stopped process) |

A `waiting` or `blocked` process also gets an inline annotation naming what it's gated on, appended directly to its name in the panel: `web (waiting on: postgres, redis)` or `web (blocked on: postgres)`, in declaration order. There's no separate detail area for this in the compact processes panel — it's shown inline because that's all the space there is.

A process with a [healthcheck](configuration.md#health-check-fields) configured shows a health dot right after its name: green `●` while healthy, red `✗` while unhealthy (distinct glyphs, so the states stay distinguishable without color). It's styled separately from the name's state color, so it stays visible regardless of process state. A process still reporting `unknown` health, or with no healthcheck configured at all, shows no dot.

A request whose response is still streaming shows its real header-time status but `...` in place of the duration until it completes; the row then updates in place (same position, no duplicate) rather than adding a new line.

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
| `/` | Search lines — jumps the cursor to a match (does not filter) |
| `n` / `N` | Jump the cursor to the next/previous match (wraps) |
| `s` | Substring filter, applied live (hides non-matching) |
| `r` | Restart the soloed process (select with `1`-`9` first) |

### Requests View

The requests view has an explicit **cursor row** (marked with `❯`). The
navigation keys move that cursor rather than scrolling raw lines, and the
viewport scrolls the minimum needed to keep the cursor on screen.

| Key | Action |
| --- | ------ |
| `j` / `↓` | Move cursor down (onto the newest row resumes auto-follow) |
| `k` / `↑` | Move cursor up (pauses auto-follow) |
| `g` / `Home` | Move cursor to the top (pauses auto-follow) |
| `G` / `End` | Move cursor to the bottom (resumes auto-follow) |
| `PgUp` / `PgDn` | Move cursor half a page |
| `F` | Toggle auto-follow (on pins the cursor to the newest row) |
| `/` | Search URL/method/subdomain — jumps the cursor to a match (does not filter) |
| `n` / `N` | Jump the cursor to the next/previous match (wraps) |
| `s` | String filter (on URL/method/subdomain) — hides non-matching rows |
| `Enter` | Open detail view for the cursor row |
| `Esc` | Return from detail, or clear the filter and search |

**Auto-follow** pins the cursor to the newest row and keeps the list scrolled
to the bottom as requests arrive. Moving the cursor up (`k`, `PgUp`, `g`)
pauses follow; moving it back onto the newest row (`j`/`PgDn`), or pressing
`G`/`End`/`F`, resumes it. While follow is paused, arriving requests never move
the cursor off the row you selected.

**Scroll-back.** The TUI loads the newest 1000 requests when it connects.
Moving the cursor onto the oldest row loads the next older page automatically
(1000 at a time, up to the 5000 the server retains). The status bar reports
`loading older…` while a page is in flight, `start of history` once the oldest
retained request is loaded, and `⚠ older: …` if a page could not be fetched —
move up again to retry. Filters and searches never reach the fetch: they apply
to what is displayed, not to what is loaded. Reconnecting re-syncs against the
server's current window and discards pages older than it, so a scrolled-back
list can never show a gap; page back down again after a reconnect.

**Search vs. filter.** `/` in the requests view *navigates* — it jumps the
cursor to matching rows and leaves every row visible — so it composes with an
active `s` filter (matches are computed over the filtered list) instead of
replacing it. `s` still *filters*, hiding non-matching rows. The status bar
shows the search indicator as `/<query> (i/k)` (cursor on match _i_ of _k_) or
`/<query> (k matches)` when the cursor is not on a match.

## Request Detail View

Press `Enter` on a request to see headers and captured bodies:

```text
Request Body (35 bytes, application/json)
  {
    "user": "alice"
  }
```

- The body section title shows the byte count, Content-Type, and (when the
  body was transfer-encoded, e.g. gzip) Content-Encoding — omitted when not
  present.
- Bodies are pretty-printed 2-space indented JSON when the Content-Type
  contains `json`, or when the raw body is itself valid JSON. Any other text
  renders unchanged except that ASCII control characters (other than tab and
  newline) and DEL are sanitized to the Unicode replacement character, so a
  captured body cannot emit ESC/BEL/OSC sequences that manipulate the terminal.
  Binary bodies show `[binary data]`; bodies that could no longer be loaded
  (e.g. evicted from disk) show `(body no longer available)`.
- A request still streaming its response shows `Duration: (in flight)` and,
  since headers/bodies haven't been captured yet, `(request in flight —
  details arrive on completion)` instead of the usual body sections.
- Detail views live-update: when the open request completes, the view
  re-fetches the request in the background and refreshes in place with its
  final duration, headers, and bodies — no loading flicker, and your scroll
  position is preserved. If that background re-fetch fails, the last snapshot
  stays on screen and the note changes to `(live refresh failed — press esc
  and re-enter to reload)`.

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

Press `/` to enter search mode. An input field appears at the bottom; the
behavior depends on the active view:

- **Logs view:** `/` is case-insensitive substring *navigation* over the log
  line text. On `Enter` the cursor jumps to the first match at-or-after its
  current position (wrapping); `n`/`N` then jump to the next and previous
  matches (wrapping). No lines are hidden, so search composes with an active
  `s` filter — the cursor row is marked with `❯` and the matched substring is
  highlighted, and the status bar shows the match indicator (`/<query> (i/k)`
  or `/<query> (k matches)`). Use `s` — not `/` — to hide non-matching lines.
- **Requests view:** `/` is case-insensitive substring *navigation* over
  URL/method/subdomain. On `Enter` the cursor jumps to the first match
  at-or-after its current row (wrapping); `n`/`N` then jump to the next and
  previous matches (wrapping). No rows are hidden, so search composes with an
  active `s` filter.
- `Esc` cancels the input; in normal mode `Esc` clears the committed
  filter/search.

## String Filter Mode

Press `s` to enter string filter mode:

- Input field appears at bottom
- Only lines matching the filter are shown
- Non-matching lines are hidden
- Header shows active filter: `logs (filter: "ERROR")`
- `Esc` clears the filter

## Help Overlay

Press `?` to show all keybindings in a modal overlay. Press any key to dismiss.
