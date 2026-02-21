# Configuration Reference

## Config File

prox looks for `prox.yaml` in the current directory by default. Use `--config` to specify a different path.

## Minimal Example

```yaml
processes:
  web: npm run dev
  api: go run ./cmd/server
  worker: python worker.py
```

## Full Example

```yaml
api:
  port: 5555
  host: 127.0.0.1

env_file: .env

processes:
  # Simple form - string command
  web: npm run dev
  worker: python worker.py

  # Expanded form - full configuration
  api:
    cmd: go run ./cmd/server
    env:
      PORT: "8080"
      DEBUG: "true"
    env_file: .env.api
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
```

## Top-Level Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `api.port` | int | dynamic | HTTP API port (auto-assigned if not specified or port in use) |
| `api.host` | string | `127.0.0.1` | API bind address |
| `env_file` | string | — | Global .env file path, loaded for all processes |
| `processes` | map | required | Process definitions |

## Process Fields

Processes can be defined in simple form (string) or expanded form (object).

### Simple Form

```yaml
processes:
  web: npm run dev
```

### Expanded Form

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run |
| `env` | map | — | Environment variables for this process |
| `env_file` | string | — | Process-specific .env file |
| `healthcheck` | object | — | Health check configuration |

## Health Check Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string | required | Command to run for health check |
| `interval` | duration | `10s` | Time between health checks |
| `timeout` | duration | `5s` | Timeout for each check |
| `retries` | int | `3` | Consecutive failures before marking unhealthy |
| `start_period` | duration | `30s` | Grace period after startup before checks begin |

## Environment Variable Precedence

Environment variables are loaded in this order (later values override earlier):

1. System environment
2. Global `env_file` (if specified)
3. Process-specific `env_file` (if specified)
4. Process-specific `env` map (if specified)

## Duration Format

Duration fields accept Go duration strings:

- `5s` - 5 seconds
- `10m` - 10 minutes
- `1h30m` - 1 hour 30 minutes

## Runtime State

When prox is running (in either foreground or daemon mode), runtime state is stored in the `.prox/` directory within your project:

| File | Description |
|------|-------------|
| `.prox/prox.state` | JSON file with port, PID, host, start time, config path |
| `.prox/prox.pid` | Process ID with file locking to prevent multiple instances |
| `.prox/prox.log` | Daemon logs (stdout/stderr redirected here in background mode) |

When running in daemon mode (`prox up -d`), all output that would normally go to stdout/stderr is redirected to `.prox/prox.log`. This is useful for debugging startup issues or reviewing daemon activity.

**State File Format:**

```json
{
  "pid": 12345,
  "port": 5555,
  "host": "127.0.0.1",
  "started_at": "2024-01-15T10:30:00Z",
  "config_file": "prox.yaml"
}
```

CLI commands automatically discover the API address by reading `.prox/prox.state`. This enables:

- Running multiple prox instances (different projects) simultaneously
- Dynamic port allocation without port conflicts
- No need to specify `--addr` for local commands

The `.prox/` directory is project-local, so add it to your `.gitignore`:

```gitignore
.prox/
```

## Proxy Configuration

prox can act as an HTTP and/or HTTPS reverse proxy, providing friendly subdomain URLs for your services. HTTP-only mode requires no certificate setup. HTTPS mode uses locally-trusted certificates via mkcert.

### HTTP-Only Example

The simplest proxy setup — no certificates required:

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  http_port: 6788
  domain: local.myapp.dev

services:
  app: 3000
  api: 8000
```

With this configuration:
- `http://app.local.myapp.dev:6788` → `http://localhost:3000`
- `http://api.local.myapp.dev:6788` → `http://localhost:8000`

### HTTPS Example

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  https_port: 6789
  domain: local.myapp.dev

services:
  app: 3000
  api: 8000

certs:
  dir: ~/.prox/certs
  auto_generate: true
```

With this configuration:
- `https://app.local.myapp.dev:6789` → `http://localhost:3000`
- `https://api.local.myapp.dev:6789` → `http://localhost:8000`

### Dual-Stack Example

Run both HTTP and HTTPS simultaneously:

```yaml
processes:
  frontend: npm run dev
  backend: go run ./cmd/server

proxy:
  http_port: 6788
  https_port: 6789
  domain: local.myapp.dev

services:
  app: 3000                    # Simple: subdomain → port
  api:                         # Expanded: with options
    port: 8000
    host: localhost

certs:
  dir: ~/.prox/certs
  auto_generate: true
```

### Proxy Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `proxy.enabled` | bool | auto | Enable reverse proxy (auto-enabled when a port is set) |
| `proxy.http_port` | int | — | Port for the HTTP proxy server |
| `proxy.https_port` | int | `6789` | Port for the HTTPS proxy server (default when enabled with no ports set) |
| `proxy.domain` | string | required | Base domain for subdomain routing |

### Service Fields

Services can be defined in simple form (port only) or expanded form (object).

#### Simple Form

```yaml
services:
  app: 3000
```

#### Expanded Form

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `port` | int | required | Target port to proxy to |
| `host` | string | `localhost` | Target host to proxy to |

### Certificate Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `certs.dir` | string | `~/.prox/certs` | Directory for storing certificates |
| `certs.auto_generate` | bool | `true` | Automatically generate certificates using mkcert |

### Prerequisites (HTTPS only)

HTTPS mode requires [mkcert](https://github.com/FiloSottile/mkcert) for certificate generation. HTTP-only mode has no prerequisites.

```bash
# macOS
brew install mkcert

# Linux
# See https://github.com/FiloSottile/mkcert#installation

# Install the CA (run once)
mkcert -install
```

### DNS Setup

Add entries to `/etc/hosts` for your subdomains:

```bash
# View required entries
prox hosts --show

# Add entries (requires sudo)
prox hosts --add
```

## Security Note

Commands in `prox.yaml` are executed via shell. Only use configuration files from trusted sources, similar to Makefiles or Procfiles.

When binding to non-localhost interfaces (`host: 0.0.0.0`), authentication is automatically enabled. A bearer token is generated and stored in `~/.prox/token`.
