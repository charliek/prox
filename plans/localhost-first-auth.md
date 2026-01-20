# Simplified Auth: Localhost-First Security

## Problem

The current authentication implementation requires a token for all API access, even when binding to localhost only. This adds unnecessary friction for local development since:
- Only local processes can connect to 127.0.0.1 anyway
- Users must manage token files
- CLI commands require auth even on the same machine

## Proposed Solution

**Bind-based security model:**
- `host: 127.0.0.1` (default) → No auth required, only local access possible
- `host: 0.0.0.0` → Auth required, token generated automatically

This provides security-by-default for network exposure while keeping local dev frictionless.

---

## Config Design

```yaml
api:
  port: 5555
  host: 127.0.0.1    # default: localhost only, no auth
  # host: 0.0.0.0    # all interfaces: auth auto-enabled
  # auth: true       # optional: explicit override
```

### Behavior Matrix

| host | auth config | Effective Auth | Token Generated |
|------|-------------|----------------|-----------------|
| 127.0.0.1 | (unset) | **disabled** | no |
| 127.0.0.1 | `true` | enabled | yes |
| 0.0.0.0 | (unset) | **enabled** | yes |
| 0.0.0.0 | `false` | disabled + **warning** | no |

### Console Output Examples

**Localhost (default):**
```
Starting prox with config: prox.yaml
API server: http://127.0.0.1:5555 (local only, no auth)
```

**All interfaces:**
```
Starting prox with config: prox.yaml
API server: http://0.0.0.0:5555 (network accessible, auth enabled)
Auth token saved to: ~/.prox/token
```

**User disables auth on 0.0.0.0:**
```
Starting prox with config: prox.yaml
WARNING: Auth disabled while binding to all interfaces (0.0.0.0)
         Any network client can control this supervisor.
API server: http://0.0.0.0:5555
```

---

## Files to Modify

### 1. `internal/config/config.go`
- Add `Auth *bool` field to `APIConfig` (pointer to detect unset vs explicit false)

### 2. `internal/api/server.go`
- Update `ServerConfig` to use `AuthEnabled bool` instead of `Token string`
- Modify `authMiddleware` to skip entirely when auth is disabled
- Keep CORS restricted to localhost (regardless of bind address)

### 3. `internal/cli/up.go`
- Calculate effective auth based on host + config override
- Only generate/save token when auth is enabled
- Print appropriate startup messages

### 4. `internal/cli/client.go`
- Only attempt to load token if it exists (already does this)
- No changes needed - client gracefully handles missing token

---

## Implementation Details

### Config Struct Change

```go
// internal/config/config.go
type APIConfig struct {
    Port int    `yaml:"port"`
    Host string `yaml:"host"`
    Auth *bool  `yaml:"auth,omitempty"`  // nil = auto-determine from host
}
```

### Auth Determination Logic

```go
// internal/cli/up.go
func isAuthRequired(cfg *config.Config) bool {
    // Explicit config takes precedence
    if cfg.API.Auth != nil {
        return *cfg.API.Auth
    }
    // Auto-determine: auth required unless localhost
    return !isLocalhost(cfg.API.Host)
}

func isLocalhost(host string) bool {
    return host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1"
}
```

### Server Config Simplification

```go
// internal/api/server.go
type ServerConfig struct {
    Host        string
    Port        int
    AuthEnabled bool   // replaces Token
    Token       string // only set if AuthEnabled
}
```

---

## Design Decisions

- **Warning on insecure config**: Print warning but allow startup when auth disabled on 0.0.0.0
- **Token location**: Keep global `~/.prox/token` for simplicity
- **CORS**: Always restrict to localhost origins only (no config needed)

---

## Verification

```bash
# Test 1: Default localhost - no auth needed
prox up
curl http://localhost:5555/api/v1/status  # Should work without auth

# Test 2: Explicit 0.0.0.0 - auth required
# In prox.yaml: api.host: 0.0.0.0
prox up
curl http://localhost:5555/api/v1/status  # Should return 401
TOKEN=$(cat ~/.prox/token)
curl -H "Authorization: Bearer $TOKEN" http://localhost:5555/api/v1/status  # Should work

# Test 3: Explicit auth on localhost
# In prox.yaml: api.auth: true
prox up
curl http://localhost:5555/api/v1/status  # Should return 401

# Test 4: Run all existing tests
go test -race ./...
```
