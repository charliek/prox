# Phase 5: Health Checks

## Status: COMPLETE

## Objective

Add health check support to monitor process health via configurable commands.

## Tasks

### Health Runner (`internal/supervisor/health.go`)

- [ ] HealthChecker struct
- [ ] Execute health check command
- [ ] Honor interval, timeout, retries
- [ ] Honor start_period (grace period)
- [ ] Track consecutive failures
- [ ] Mark healthy/unhealthy transitions
- [ ] Capture health check output

### Supervisor Integration

- [ ] Start health checker per process with healthcheck config
- [ ] Update ProcessInfo with health state
- [ ] Emit health change events
- [ ] Stop health checker on process stop

### API Updates (`internal/api/handlers.go`)

- [ ] Include health info in GET /processes
- [ ] Include healthcheck details in GET /processes/{name}

### Domain Updates (`internal/domain/health.go`)

- [ ] HealthStatus enum (healthy, unhealthy, unknown)
- [ ] HealthState struct (last_check, last_output, consecutive_failures)

## Health Check Config

```yaml
processes:
  api:
    cmd: go run ./cmd/server
    healthcheck:
      cmd: curl -f http://localhost:8080/health
      interval: 10s      # Time between checks
      timeout: 5s        # Check timeout
      retries: 3         # Failures before unhealthy
      start_period: 30s  # Grace period on startup
```

## Health State Machine

```
        start_period
           ends
┌────────┐      ┌─────────────┐
│starting│─────▶│   checking  │◀──────┐
└────────┘      └──────┬──────┘       │
                       │              │
              ┌────────┴────────┐     │
              ▼                 ▼     │
        ┌─────────┐       ┌──────────┐│
        │ healthy │◀─────▶│unhealthy ││
        └─────────┘       └──────────┘│
              │                 │     │
              └─────────────────┴─────┘
                    interval
```

## Health Check Flow

1. Process starts → health = "unknown"
2. Wait for start_period
3. Run health check command
4. If success → mark healthy
5. If failure → increment consecutive_failures
6. If consecutive_failures >= retries → mark unhealthy
7. One success resets to healthy
8. Repeat at interval

## API Response Updates

### GET /processes/{name}
```json
{
  "name": "api",
  "status": "running",
  "pid": 12345,
  "uptime_seconds": 3600,
  "restarts": 2,
  "health": "healthy",
  "healthcheck": {
    "enabled": true,
    "last_check": "2025-01-19T10:32:01.123Z",
    "last_output": "OK",
    "consecutive_failures": 0
  }
}
```

## Test Config

```yaml
# testdata/configs/healthcheck.yaml
processes:
  healthy:
    cmd: ./testdata/scripts/long_running.sh
    healthcheck:
      cmd: "true"
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s

  unhealthy:
    cmd: ./testdata/scripts/long_running.sh
    healthcheck:
      cmd: "false"
      interval: 1s
      timeout: 1s
      retries: 2
      start_period: 1s
```

## Verification

```bash
go test ./internal/supervisor/... -v

# Manual verification
# Create test config with healthcheck
./prox up
curl http://localhost:5555/api/v1/processes/api
# Verify health transitions from unknown → healthy
# Kill the health endpoint, verify unhealthy transition
```

## Deviations & Notes

_Document any changes or notable items here._

| Item | Description |
|------|-------------|
| | |

## Completion Checklist

- [ ] HealthChecker runs commands at interval
- [ ] start_period delays initial checks
- [ ] timeout kills slow health checks
- [ ] retries threshold triggers unhealthy
- [ ] One success resets to healthy
- [ ] ProcessInfo includes health status
- [ ] API includes healthcheck details
- [ ] Health checker stops with process
- [ ] Tests cover all transitions
