# Quick Start

## Installation

Install with Go:

```bash
go install github.com/charliek/prox/cmd/prox@latest
```

Or build from source:

```bash
git clone https://github.com/charliek/prox.git
cd prox
go build -o prox ./cmd/prox
```

## Create Configuration

Create a `prox.yaml` in your project directory:

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

## Start Processes

Start all processes:

```bash
prox up
```

In a terminal this opens the [interactive TUI](#interactive-tui), showing every process and its logs live. Piped, redirected, or in CI it streams aggregated logs from all processes with color-coded prefixes instead; `prox up --no-tui` asks for that on a terminal too.

## Check Status

In another terminal, check process status:

```bash
prox status
```

Output:

```
NAME     STATUS    PID    UPTIME     RESTARTS  HEALTH
web      running   12345  5m30s      0         -
api      running   12346  5m30s      0         -
worker   running   12347  5m30s      1         -
```

`HEALTH` is `-` because none of these processes declares a
[`healthcheck`](../reference/configuration.md#health-check-fields) — prox had nothing
to run, so it reports nothing. Once you add one, the column shows `healthy` or
`unhealthy`, or `unknown` while a configured check has yet to report.

## View Logs

View recent logs:

```bash
prox logs
```

Stream logs continuously:

```bash
prox logs -f
```

Filter logs by process:

```bash
prox logs --process api
```

## Interactive TUI

A foreground `prox up` already gives you the interactive terminal UI — that is what you saw when you ran it above:

```bash
prox up
```

It opens whenever the terminal can host one, and quietly streams plain logs instead when it cannot: under a pipe or a redirect, in CI, with `TERM` unset or `dumb`, or in a backgrounded `prox up &`. To stream plain logs on a normal terminal too, pass `--no-tui` (or set `PROX_TUI=0` for the whole shell); to insist on the TUI and get an error rather than a fallback, pass `--tui`.

Quitting with `q` stops your processes, exactly as Ctrl-C does — the foreground `prox up` is their supervisor. To keep them running and watch them whenever you like, start detached and attach:

```bash
prox up -d
prox attach
```

It is the same TUI either way: `prox up` runs it against the process's own API, exactly as `prox attach` does against a daemon. The only difference is ownership — quitting `attach` leaves the daemon running, and the footer says which mode you are in (`q stop` vs `q quit`).

The TUI provides:

- Real-time log viewing with scrollback
- Menu bar (View / Filter / Theme), theme cycling (`t`), and view toggles
- Process filtering via `1-9`, process chips, and `proc:` filter clauses
- Search with `/` and filter query language with `s` (Filter menu via `f`)
- Process restart with `r`
- Mouse: wheel scroll, row/chip clicks, double-click request rows for detail
- Press `?` for help, `q` to quit (stopping the processes under `prox up`, detaching under `prox attach`)

## Background Mode

Run prox as a background daemon:

```bash
# Start in background
prox up -d

# Check status
prox status

# View logs
prox logs -f

# Attach TUI to running daemon
prox attach

# Stop the daemon
prox down
```

Background mode features:

- Processes continue running after terminal closes
- Multiple prox instances can run (different projects, different ports)
- CLI commands auto-discover the running daemon
- Daemon logs are written to `.prox/prox.log`

## Proxy (Optional)

prox can provide friendly subdomain URLs for your services via HTTP and/or HTTPS reverse proxying.

### HTTP Proxy (simplest)

No certificate or DNS setup required when using `lvh.me` (resolves to `127.0.0.1` automatically):

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  http_port: 6788
  domain: lvh.me

services:
  app: 3000
  api: 8000
```

### HTTPS Proxy

For locally-trusted HTTPS, install mkcert first:

```bash
# macOS
brew install mkcert

# Install the CA (run once)
mkcert -install
```

Then configure HTTPS:

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  https_port: 6789
  domain: lvh.me

services:
  app: 3000
  api: 8000
```

### Usage

Start prox:

```bash
prox up
```

Access your services:

- `http://app.lvh.me:6788` → `http://localhost:3000` (HTTP mode)
- `https://app.lvh.me:6789` → `http://localhost:3000` (HTTPS mode)

When several projects configure the same proxy port, prox uses a shared proxy daemon automatically. Each project can register its own hostnames on the same port, including `443`, and keep an independent `prox up` / `prox down` lifecycle.

```bash
prox proxy routes
```

See the [Shared Proxy Across Projects](../guides/shared-proxy.md) guide for multi-project routing. See the [Local DNS & Certificates](../guides/local-dns.md) guide for custom domains, certificate management, and sharing CAs across machines. See the [Configuration Reference](../reference/configuration.md#proxy-configuration) for full proxy options.

Once the proxy is enabled, `prox requests` and `prox requests <id> --body` work immediately — request/response capture is on by default, with headers, query params, and bodies (up to `max_body_size`) all recorded exactly as sent — no values are altered or hidden. See [Request Capture](../reference/configuration.md#request-capture) for the disk budget, the cleartext posture, and how to opt out.

## HTTP API

By default, the API binds to a dynamic (auto-assigned) localhost port chosen at startup. Run `prox status` to see the address currently in use, or pin a fixed port by adding this to `prox.yaml`:

<!-- doclint:pin-example -- deliberate api.port pinning demonstration, see internal/docslint -->
```yaml
api:
  port: 5555
```

With `api.port: 5555` set, check supervisor status:

```bash
curl http://localhost:5555/api/v1/status
```

List processes:

```bash
curl http://localhost:5555/api/v1/processes
```

See the [API Reference](../reference/api.md) for all endpoints.
