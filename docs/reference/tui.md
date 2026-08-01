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

The TUI has two main views you can switch between with `Tab` (or the View menu):

- **Logs View** — Real-time process logs with filtering
- **Requests View** — Real-time HTTP proxy requests (when proxy is enabled)

Press `Enter` on a request row (or double-click it) to open **Request Detail**.

## Layout

With the default chrome (menu bar and process panel visible):

```text
 prox  myproject                    View ▾  Filter ▾  Theme ▾
┌─ processes ──────────────────────────────────────────────┐
│ 1: ● web     running   2: ● api    running               │
├─ logs ───────────────────────────────────────────────────┤
│ 10:32:01 web    │ GET /api/users 200 12ms                │
│ 10:32:02 api    │ connected to database                  │
│ ...                                                      │
├──────────────────────────────────────────────────────────┤
│ [Logs] [FOLLOW] 45 lines                                 │
│ ? help · m menu · / search · s filter · q quit           │
└──────────────────────────────────────────────────────────┘
```

Toggle the menu bar with `m`, the process panel with `p`. View preferences persist in `~/.prox/tui/config.toml`.

## Themes

Six built-in presets ship with prox: `tokyo-night` (default), `dark`, `light`, `catppuccin`, `gruvbox`, and `legacy` (approximates the pre-redesign ANSI look).

- Press `t` to cycle presets and any user themes (sorted by filename).
- Open the **Theme** menu from the menu bar (click `Theme ▾`, or `v` then `→`/`Tab` to sibling-switch) to pick a theme by name.
- Set a default in config: `theme = "catppuccin"` in `~/.prox/tui/config.toml`.
- Add custom themes as TOML files in `~/.prox/tui/themes/<name>.toml`:

```toml
base = "tokyo-night"

[colors]
bg = "#1a1b26"
fg = "#a9b1d6"

[palette]
# optional 9-entry process colour list (hex)
```

Each file names a `base` preset and may override `[colors]` (snake_case hex) and `[palette]`. Malformed files fall back to the default with a warning in the log pane.

HTTP status colours, log level tints, and JSON syntax highlighting all follow the active theme.

## Process State Colours

The processes panel colour-codes each process name by its state, including the states introduced by [dependencies and tasks](../guides/dependencies.md). Colours are theme-defined (not fixed ANSI indices); the mapping is semantic:

| State | Typical colour role |
| ----- | ------------------- |
| `running` | success / OK |
| `stopped` | dim |
| `crashed` | error |
| `starting` / `stopping` | warning |
| `waiting` | amber (distinct from starting) |
| `blocked` | error, bold |
| `completed` | dim |

A `waiting` or `blocked` process also gets an inline annotation naming what it's gated on. A process with a [healthcheck](configuration.md#health-check-fields) shows a health dot after its name: green `●` while healthy, red `✗` while unhealthy.

Click a process chip in the panel (logs view) to solo that process — the same as pressing its `1`–`9` key. Click again to clear solo.

## Keybindings

### General

| Key | Action |
| --- | ------ |
| `Tab` | Switch between Logs and Requests |
| `↑` / `↓` / `j` / `k` | Scroll (logs) or move cursor (requests) |
| `PgUp` / `PgDn` | Page scroll / half-page cursor step |
| Scroll wheel | Scroll logs or move requests cursor (3 lines per notch) |
| `Home` / `End` / `g` / `G` | Jump to start/end (or cursor top/bottom in requests) |
| `F` | Toggle auto-follow |
| `?` | Help modal |
| `m` | Toggle menu bar |
| `v` | Open View menu (when bar visible) |
| `f` | Open Filter menu (when bar visible) |
| `t` | Cycle theme |
| `p` | Toggle process panel |
| `T` | Toggle timestamps in log lines |
| `w` | Toggle soft-wrap in logs |
| `Esc` | Clear filters/search/solo; back from detail |
| `q` | Quit |

### Menu bar

When the menu bar is visible:

- Click `View ▾`, `Filter ▾`, or `Theme ▾` to open a dropdown.
- Hover a sibling menu cell while a dropdown is open to slide the menu across
  the bar; hover dropdown rows to move the highlight.
- With a menu open: `←`/`→`/`Tab` switch between menus; `↑`/`↓`/`j`/`k` navigate rows (separators skipped); the scroll wheel moves the highlight; `Enter`/`Space` activate; any other key closes the menu (except `?`, which closes the menu and opens help).
- Long menus clamp to the frame with “… N more …” indicator rows; wheel scrolls the visible window.
- Opening a menu by mouse while typing in a filter/search bar blurs the input first.

Menu choices persist view toggles and theme to `~/.prox/tui/config.toml` (`theme` plus `[view]` keys: `process_panel`, `timestamps`, `wrap`, `menu_bar`).

### Logs View

| Key | Action |
| --- | ------ |
| `1-9` | Solo process (toggle) |
| `s` | Filter bar — query language, applied live |
| `f` | Filter menu — process checks + log levels |
| `/` | Search — jumps cursor to match (does not filter) |
| `n` / `N` | Next/previous search match |
| `y` | Copy parked line (after click or `/` search cursor) |
| `r` | Restart the soloed process |
| Mouse click line | Park cursor on that entry (disengages follow) |

**Filter query language (logs):** space-separated tokens. Examples:

```text
proc:api level:error -health
proc:web proc:worker level:warn
re:timeout
```

Fields: `proc:` (repeatable), `level:error|warn|info|debug|trace` (repeatable), `re:<regex>` (≤256 chars). Bare words are case-insensitive AND substring matches on the line. `-` negates any token. Invalid syntax keeps the last good filter and shows a hint in the status bar.

The **Filter menu** edits the same state as the `s` bar; menu changes rewrite the bar text canonically.

### Requests View

The requests view has an explicit **cursor row** (marked with `❯`). Navigation moves that cursor; the viewport scrolls to keep it visible.

| Key | Action |
| --- | ------ |
| `j` / `↓` | Move cursor down (onto newest resumes follow) |
| `k` / `↑` | Move cursor up (pauses follow) |
| `g` / `Home` | Cursor to top |
| `G` / `End` | Cursor to bottom (resumes follow) |
| `s` | Filter bar — query language |
| `f` | Filter menu — status class + methods |
| `/` | Search URL/method/subdomain (navigate only) |
| `n` / `N` | Next/previous match |
| `Enter` | Open detail for cursor row |
| `y` | Copy full request ID |
| `c` | Copy as curl |
| Mouse click | Select row; double-click opens detail |

**Filter query language (requests):** examples:

```text
method:GET status:5xx host:api url:/orders
sub:web method:POST
```

Fields: `sub:`, `method:` (repeatable), `status:` (`500`, `5xx`, `>=400`, `200-299`, …), `url:` (repeatable). Bare terms OR-match URL, method, and subdomain (back-compat with the old substring filter).

**Auto-follow** pins the cursor to the newest row. Scrolling or moving up pauses follow; `G`/`End`/`F`, or moving onto the newest row, resumes it.

**Scroll-back.** The TUI loads the newest 1000 requests on connect. Moving the cursor to the oldest visible row fetches older pages (1000 at a time). Filters apply client-side to loaded data only.

### Request Detail View

| Key | Action |
| --- | ------ |
| `Esc` | Back to requests list |
| `y` | Copy request ID |
| `c` | Copy as curl (`https://<host>[:port]<path>`; port included in `up --tui`, omitted in attach) |
| `Y` | Copy wire JSON (exact API payload) |
| Scroll wheel | Scroll detail content |

Detail views live-update when the open request completes.

## Search vs. Filter

- **`/`** navigates: jumps the cursor to matches without hiding rows/lines. Composes with an active `s` filter.
- **`s`** filters: hides non-matching entries using the query language above. Each view keeps its own filter across `Tab` switches.
- Status bar shows committed search as `/<query> (i/k)` or filter as `Filter: <query>`.

## Copy (grab-for-agent)

Copy keys write to the system clipboard (with a status flash on success or failure):

| Context | Key | Copies |
| ------- | --- | ------ |
| Requests list | `y` | Full request ID |
| Requests list | `c` | curl command |
| Detail | `y` / `c` / `Y` | ID / curl / wire JSON |
| Logs (parked cursor) | `y` | Raw log line |

## Mouse

Mouse support requires a terminal that reports all motion (`tea.WithMouseAllMotion`,
SGR `?1003` + `?1006`).

| Action | Effect |
| ------ | ------ |
| Wheel (no menu open) | 3 lines per notch — scroll logs, move requests cursor, or scroll detail |
| Wheel (menu open) | Move dropdown highlight; viewport does not scroll |
| Click process chip | Solo/unsolo (logs view) |
| Click log line | Park cursor; disengages follow |
| Click request row | Move cursor |
| Double-click request row | Open detail (~500 ms window) |
| Click / hover menu bar | Open menus; hover slides an open menu across siblings |
| Click dropdown row | Activate item |
| Click while typing filter/search | Ignored (except menu bar, which blurs input) |

Wheel events are handled entirely by the TUI (the bubbles viewport wheel handler is disabled) so scrolling does not double-fire.

**Text selection:** the TUI owns the pointer for mouse reporting. To select text in
the terminal, use your terminal’s override modifier (commonly **Shift**-click or
**Option**-click on macOS — the exact key is terminal-specific).

## Help modal

Press `?` in Normal or Help mode for context-sensitive help (logs, requests, or
detail). Help appears as a centered modal over the live UI (the menu bar, status
bar, and streaming content stay visible behind it). Scroll with `j`/`k`, PgUp/PgDn,
`g`/`G`, or the wheel over the box when content is taller than the modal. Close
with Esc, `?`, `q`, Enter, or a click outside the box.
