# TUI Reference

The interactive terminal UI provides real-time log viewing with filtering and search, plus a proxy request viewer when the proxy is enabled.

## Starting the TUI

A foreground `prox up` opens the TUI whenever the terminal can host one — no flag needed:

```bash
prox up
```

Or start specific processes:

```bash
prox up web api
```

For a daemon started with `prox up -d`, attach to it instead:

```bash
prox attach
```

It is the same TUI either way. The difference is ownership: `prox up` **is** the supervisor, so quitting it stops your processes, while `prox attach` only watches a daemon and quitting leaves it running. The footer and the `?` help modal say which one you are in (`q stop` vs `q quit`).

`prox up` falls back to plain log streaming when the terminal cannot host a TUI (a pipe or redirect, CI, `TERM` unset or `dumb`, a backgrounded `prox up &`). `--no-tui` or `PROX_TUI=0` opts out on a capable terminal; `--tui` requires the TUI and errors instead of falling back. See [TUI mode resolution](cli.md#up).

## Views

The TUI has two main views you can switch between with `Tab` (or the View menu):

- **Logs View** — Real-time process logs with filtering
- **Requests View** — Real-time HTTP proxy requests (when proxy is enabled)

Press `Enter` on a request row (or double-click it) to open **Request Detail**.

## Layout

With the default chrome (menu bar and process panel visible):

```text
 prox  myproject                    View ▾  Filter ▾  Theme ▾
 1: ● web     running   2: ● api    running
╭─ Logs ───────────────────────────────────────────────────╮
│ 10:32:01 web    connected to database                    │
│ 10:32:02 api    │ GET /api/users 200 12ms                 │
│ ...                                                      │
╰──────────────────────────────────────────────────────────╯
 Tab: switch view | ? for help   [Logs] [FOLLOW] 45/120 lines   ? help · m menu · / search · s filter · q stop
```

The **process panel** sits above the main content. The **viewport** (logs, requests list, or detail body) is wrapped in a rounded border with a title spliced into the top edge (`─ Logs ─`, `─ Requests ─`, or `─ Request <id> ─`). On very small terminals the panel may render borderless rather than corrupt the frame.

Toggle the menu bar with `m`, the process panel with `p`. View preferences persist in `~/.prox/tui/config.toml`.

### Merged footer

A single footer row carries everything that used to split across status and hint rows:

- **Left:** typed status — mode prompts (`Search:` / `Filter:` while typing — search applies live, jumping the cursor to matches on every keystroke), committed search/filter/solo text, connection or restart messages, or the idle hint. Errors render as `✗ …` on the footer background.
- **Right:** `[Logs]` / `[Requests]` / `[Request Detail]`, `[FOLLOW]` / `[PAUSED]`, visible/total count, then two-tone key hints (`? help · m menu · / search · s filter · q stop`).

The last hint is mode-aware: `q stop` under `prox up`, where quitting stops the processes, and `q quit` under `prox attach`, where it only detaches. The `?` help modal spells the same thing out in full.

On narrow terminals hints drop right-to-left (non-sticky pairs first; `? help` and the `q` pair last), then the count, then the status text truncates. Clicks on the footer are consumed and do nothing.

### Bordered chrome

- **Dropdown menus** use rounded borders. Each row shows a right-aligned **hint** column (keyboard shortcut) when space allows; long menus clamp with “… N more …” rows.
- **Help modal** is a centred rounded box with a **focused** border colour and the view name spliced into the top border (`─ Help — Logs ─`). On very narrow frames side padding and then the border degrade before content.

### Dead-stack banner

When every process has stopped and at least one of them `crashed` or was `blocked` on a failed dependency, a persistent full-width row appears between the process panel and the viewport's top border:

```text
 All processes have stopped — 2 crashed. Nothing is running. Press q to quit.
```

The TUI does **not** exit on its own here — unlike a plain, piped `prox up`, which does (see [the foreground exit contract](cli.md#up)) — because the user is present and reading, and pulling the screen away would take the crash output with it. The sentence names both `crashed` and `blocked` counts (never just "crashed", if a launch was actually blocked by a failed dependency) and, like the process labels above, is readable with every escape code stripped — colour is emphasis only. It appears identically in `prox up` and `prox attach`, and clears itself the moment any process becomes live again (a `restart`, for example).

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

On **FullFill** presets (all except `legacy`), the active cursor row paints a full-width **selection band** across every wrapped display row of that entry. `/` search hits inside the band keep their search-highlight colour on top of the band.

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

**Colour is emphasis, not the only signal.** `crashed`/`blocked` share one colour role, as do `stopped`/`completed`, so those pairs are only colour-distinguishable — nothing at all once ANSI is stripped (piped output, `TERM=dumb`, a screenshot) or to a colour-blind reader. Every state but `running` therefore appends a parenthesized word to the process name, and that label is what actually survives stripping:

Each suffix is appended after the process name, separated by a space.

| State | Name suffix |
| ----- | ------------ |
| `running` | *(none)* |
| `crashed` | `(crashed)` |
| `blocked` | `(blocked)`, or `(blocked on: <target>[, ...])` naming the failed `depends_on` targets |
| `waiting` | `(waiting)`, or `(waiting on: <target>[, ...])` naming the still-resolving targets |
| `completed` | `(done)` |
| `stopped` | `(stopped)` |
| `starting` | `(starting)` |
| `stopping` | `(stopping)` |

A process with a [healthcheck](configuration.md#health-check-fields) shows a health dot after its name (and its state label): green `●` while healthy, red `✗` while unhealthy. A process with no healthcheck configured, or whose check has not reported yet, shows no dot.

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
| `?` | Help modal (Normal mode; see Help modal for menu-open `?`) |
| `m` | Toggle menu bar |
| `v` | Open View menu (when bar visible) |
| `f` | Open Filter menu (when bar visible) |
| `t` | Cycle theme |
| `p` | Toggle process panel |
| `T` | Toggle timestamps in log lines |
| `w` | Toggle soft-wrap in logs |
| `Esc` | Clear filters/search/solo; back from detail; while typing `/`, cancels the live search and restores the view to where `/` was pressed |
| `q` | Quit (Normal mode) — under `prox up` this also stops the processes, under `prox attach` it only detaches; close help without quitting (Help mode); typed into search/filter bars while editing |
| `ctrl+c` | Quit from any mode (including help and text entry), with the same stop-vs-detach meaning as `q` |

### Menu bar

When the menu bar is visible:

- Click `View ▾`, `Filter ▾`, or `Theme ▾` to open a dropdown.
- Hover a sibling menu cell while a dropdown is open to slide the menu across the bar; hover dropdown rows to move the highlight.
- With a menu open: `←`/`→`/`Tab` switch between menus; `↑`/`↓`/`j`/`k` navigate rows (separators skipped); the scroll wheel moves the highlight; `Enter`/`Space` activate; any other key closes the menu (except `?`, which closes the menu and opens help).
- Long menus clamp to the frame with “… N more …” indicator rows; wheel scrolls the visible window.
- Opening a menu by mouse while typing in a filter/search bar blurs the input first — for the `/` bar this commits the live search (keeps what you typed), the same as `Enter`.

Menu choices persist view toggles, theme, and (in Requests view) column visibility to `~/.prox/tui/config.toml` (`theme`, `[view]` keys: `process_panel`, `timestamps`, `wrap`, `menu_bar`, plus `[requests]` column booleans).

### Logs View

| Key | Action |
| --- | ------ |
| `1-9` | Solo process (toggle) |
| `s` | Filter bar — query language, applied live |
| `f` | Filter menu — process checks + log levels |
| `/` | Search — applied live as you type (does not filter); `Enter` keeps it, `Esc` cancels & restores; reopens seeded with the last committed search |
| `n` / `N` | Next/previous search match |
| `y` | Copy parked line (after click or `/` search cursor) |
| `r` | Restart the soloed process |
| Mouse click line | Park cursor on that entry (disengages follow) |

**Filter query language (logs):** space-separated tokens. Examples:

```text
proc:api level:error -health
proc:web proc:worker level:warn
re:timeout -re:health
re:"foo bar"
```

Fields:

- `proc:` — process name (repeatable; positives OR)
- `level:error|warn|info|debug|trace` — detected level (repeatable; positives OR; `warning`→warn, `fatal`/`critical`→error)
- `re:<pattern>` — RE2 regex, ≤256 bytes, case-sensitive (opt into `(?i)`); compiled once at parse; multiple `re:` terms AND together; `-re:` excludes matches. Matches the raw log line (same as bare terms).
- Bare words — case-insensitive AND substring matches on the raw line

`-` negates any token. Invalid syntax (including bad `re:` compile) keeps the last good filter and shows a hint in the status bar. Values with whitespace use `field:"quoted"` form; there are no escapes — a value containing both whitespace and `"` is unrepresentable.

JSON object log lines may *display* as a compact `path=value` summary (parsed once at ingest and cached; wrap-on keeps summary plus compact raw), but filter/`/` search still match the raw line.

A line's level is detected at ingest from (first match wins): a JSON `level`/`lvl`/`severity` key (string, or pino/bunyan numeric), a logfmt `level=`/`lvl=` token at line start or after whitespace, or a standalone UPPERCASE level token early in the line — the shape python logging, tracing's text format, pino-pretty, and uvicorn emit in local dev. Lines with no detectable level are excluded from positive `level:` filters (and untinted).

The **Filter menu** edits the same state as the `s` bar; menu changes rewrite the bar text canonically.

### Requests View

The requests view has an explicit **cursor row** (marked with `❯`). Navigation moves that cursor; the viewport scrolls to keep it visible.

**Empty state.** With no active filter, the empty view names *why* there are no rows rather than showing one generic message for every cause:

- No proxy running this session (no `proxy:` block, one that is `enabled: false`, or `--no-proxy`): "No proxy running — enable a proxy: block in prox.yaml to capture requests".
- A proxy is running but `proxy.capture.enabled: false`: "No requests yet — capture is off, so rows will show metadata only" — rows still arrive with capture off (method, URL, status, timing), they just carry no headers or bodies.
- Otherwise: "No requests yet — traffic through the proxy appears here".

An active filter with no matches instead shows what didn't match ("No lines match …").

Open the **View** menu (`v`) for a **Columns** checkbox section (Requests and Request Detail views): toggle Time, Host, Method, Status, Duration, and ID. **URL is always shown.** Defaults are all on; choices persist under `[requests]` in config. `/` search matches only visible columns; copy keys (`y`, `c`) are unaffected.

| Key | Action |
| --- | ------ |
| `j` / `↓` | Move cursor down (onto newest resumes follow) |
| `k` / `↑` | Move cursor up (pauses follow) |
| `g` / `Home` | Cursor to top |
| `G` / `End` | Cursor to bottom (resumes follow) |
| `s` | Filter bar — query language |
| `f` | Filter menu — status class + methods |
| `/` | Search visible columns — applied live as you type (navigate only, not filter); `Enter` keeps it, `Esc` cancels & restores; reopens seeded with the last committed search |
| `n` / `N` | Next/previous match |
| `Enter` | Open detail for cursor row |
| `y` | Copy full request ID |
| `c` | Copy as curl |
| Mouse click | Select row; double-click opens detail |

**Filter query language (requests):** examples:

```text
method:GET status:5xx host:api url:/orders
status:>=400 status:200-399 in_flight:true
```

Fields:

- `method:` — HTTP method (repeatable; case-insensitive; positives OR)
- `status:` — exact (`200`), class (`4xx`/`5XX`), inequality (`>=400`, `<=299`), or inclusive range (`200-399`). Endpoints must be 100–599; reversed ranges and malformed/partial operators are rejected with distinct errors. Positives OR; `-status:` excludes.
- `host:` — case-insensitive hostname substring (repeatable)
- `url:` — case-insensitive path+query substring (repeatable)
- `in_flight:true|false` — single-valued constraint
- Bare terms — each AND'd; a term matches if it appears in method, host, URL, or subdomain

`-` negates any token. Invalid syntax keeps the last good filter and shows a hint in the status bar.

**Auto-follow** pins the cursor to the newest row. Scrolling or moving up pauses follow; `G`/`End`/`F`, or moving onto the newest row, resumes it.

**Scroll-back.** The TUI loads the newest 1000 requests on connect. Moving the cursor to the oldest visible row fetches older pages (1000 at a time). Filters apply client-side to loaded data only.

### Request Detail View

| Key | Action |
| --- | ------ |
| `Esc` | Back to requests list |
| `y` | Copy request ID |
| `c` | Copy as curl (`https://<host>[:port]<path>`; port included under `prox up`, omitted in attach) |
| `Y` | Copy wire JSON (exact API payload) |
| Scroll wheel | Scroll detail content |

Detail views live-update when the open request completes.

**Capture-disabled records.** When `proxy.capture.enabled: false`, a completed request's detail view has no headers and no body sections to show — the record was written with no captured details — and says so rather than rendering an unexplained blank view: "Capture is disabled (proxy.capture.enabled: false) — no headers or bodies were recorded". An in-flight request (still running) shows its own, unrelated note instead, since its details simply haven't arrived yet.

## Search vs. Filter

- **`/`** navigates: applies live as you type, jumping the cursor to matches without hiding rows/lines. `Enter` keeps the result and exits the bar; `Esc` cancels — clearing the view's search, including a previously committed query the bar was seeded with, and restoring cursor, scroll, and follow to where `/` was pressed. Composes with an active `s` filter. Reopening `/` seeds the bar with the view's last committed search.
- **`s`** filters: hides non-matching entries using the query language above. Each view keeps its own filter across `Tab` switches.
- The merged footer left side shows committed search as `/<query> (i/n)` (cursor on match *i* of *n*) or `/<query> (n matches)`, filter as `Filter: <query>`, or the idle hint. While typing, `Search:` / `Filter:` prompts take precedence.

## Copy (grab-for-agent)

Copy keys write to the system clipboard (with a status flash on success or failure):

| Context | Key | Copies |
| ------- | --- | ------ |
| Requests list | `y` | Full request ID |
| Requests list | `c` | curl command |
| Detail | `y` / `c` / `Y` | ID / curl / wire JSON |
| Logs (parked cursor) | `y` | Raw log line |

## Mouse

Mouse support requires a terminal that reports all motion (`tea.WithMouseAllMotion`, SGR `?1003` + `?1006`).

On terminals that support **OSC 22** (kitty, WezTerm, Alacritty, Ghostty, …), the pointer switches to a hand over activatable targets: menu-bar cells, dropdown rows (not separators or overflow indicators), and process chips in Normal mode. Content rows, the footer, panel borders, and the help box keep the default pointer.

| Action | Effect |
| ------ | ------ |
| Wheel (no menu open) | 3 lines per notch — scroll logs, move requests cursor, or scroll detail |
| Wheel (menu open) | Move dropdown highlight; viewport does not scroll |
| Wheel (help open) | Scroll help body when the pointer is over the box |
| Click process chip | Solo/unsolo (logs view) |
| Click log line | Park cursor; disengages follow |
| Click request row | Move cursor |
| Double-click request row | Open detail (~500 ms window) |
| Click panel border or requests header | Consumed no-op |
| Click footer | Consumed no-op |
| Click / hover menu bar | Open menus; hover slides an open menu across siblings |
| Click dropdown row | Activate item |
| Click while typing filter/search | Ignored (except menu bar, which blurs input) |
| Click outside help box | Close help |

Wheel events are handled entirely by the TUI (the bubbles viewport wheel handler is disabled) so scrolling does not double-fire.

**Text selection:** the TUI owns the pointer for mouse reporting. To select text in the terminal, use your terminal’s override modifier (commonly **Shift**-click or **Option**-click on macOS — the exact key is terminal-specific).

## Help modal

Press `?` in Normal mode for context-sensitive help (logs, requests, or detail). With a menu open, `?` closes the menu and opens help. Help appears as a centred modal over the live UI (the menu bar, footer, and streaming content stay visible behind it). Scroll with `j`/`k`, PgUp/PgDn, `g`/`G`, or the wheel over the box when content is taller than the modal. Close with Esc, `?`, `q`, Enter, or a click outside the box. `ctrl+c` quits the TUI from help as well.
