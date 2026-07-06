# Local DNS & Certificates

## DNS Resolution

Custom domains like `local.myapp.dev` require DNS entries pointing to `127.0.0.1`. The simplest approach is to use a public DNS service that already resolves to localhost — no `/etc/hosts` editing needed.

| Service | Domain Pattern | Example |
|---------|---------------|---------|
| [lvh.me](http://lvh.me) | `*.lvh.me` | `app.lvh.me` |
| [sslip.io](https://sslip.io) | `*.127.0.0.1.sslip.io` | `app.127.0.0.1.sslip.io` |
| [nip.io](https://nip.io) | `*.127.0.0.1.nip.io` | `app.127.0.0.1.nip.io` |
| [localtest.me](http://localtest.me) | `*.localtest.me` | `app.localtest.me` |

These services resolve any subdomain to `127.0.0.1` automatically. They require internet access for DNS resolution.

**Example `prox.yaml` using lvh.me:**

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

With this configuration:

- `http://app.lvh.me:6788` → `http://localhost:3000`
- `http://api.lvh.me:6788` → `http://localhost:8000`

For multiple projects on the same port, keep DNS pointed at `127.0.0.1` and give each project distinct service hostnames. prox registers those hostnames with the shared proxy daemon automatically. See [Shared Proxy Across Projects](shared-proxy.md).

If you prefer a custom domain like `local.myapp.dev`, add entries to `/etc/hosts` manually:

```text
127.0.0.1 local.myapp.dev app.local.myapp.dev api.local.myapp.dev
```

## HTTPS Certificates

### One-Time Setup

Install [mkcert](https://github.com/FiloSottile/mkcert) and its CA:

```bash
# macOS
brew install mkcert

# Linux
# See https://github.com/FiloSottile/mkcert#installation

# Install the CA into your system trust store (run once)
mkcert -install
```

### Automatic Certificate Generation

When `auto_generate: true` (the default), prox calls mkcert to generate wildcard certificates on first HTTPS startup. Certificates are stored in `~/.prox/certs/` by default.

```yaml
proxy:
  https_port: 6789
  domain: lvh.me

certs:
  dir: ~/.prox/certs
  auto_generate: true   # default
```

No further action is needed — prox handles cert generation automatically.

### Manual Certificate Generation

If you set `auto_generate: false`, generate certificates yourself:

```bash
mkcert -cert-file cert.pem -key-file key.pem "*.lvh.me" "lvh.me"
```

Then point prox at the cert files via `certs.dir`.

## Sharing Your CA Across Machines

### Option A: Each Developer Creates Their Own CA (Simplest)

Each developer runs `mkcert -install` on their own machine. prox auto-generates certs per machine. No coordination needed.

### Option B: Share a CA Across Machines

For consistent trust (e.g., a team sharing a dev environment):

1. On the source machine, find the CA files:

    ```bash
    mkcert -CAROOT
    # e.g., /Users/you/Library/Application Support/mkcert
    ```

2. Copy `rootCA.pem` and `rootCA-key.pem` to the target machine securely via a secrets manager (1Password, Vault, etc.).

3. On the target machine, point mkcert at the shared CA and install it:

    ```bash
    export CAROOT=/path/to/shared/ca
    mkcert -install
    ```

!!! warning
    `rootCA-key.pem` gives complete power to intercept HTTPS traffic from any machine that trusts it. Store it in a secrets manager — **never commit it to version control**.
