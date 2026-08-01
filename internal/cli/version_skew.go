package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxyd"
)

// skewOps bundles the injectable operations the version-skew recovery path
// needs so recoverFromVersionSkew can be unit-tested against a fake daemon
// without real sockets or wall-clock waits. defaultSkewOps wires the production
// implementations.
type skewOps struct {
	// statusProbe queries the (old) daemon's /status with a short timeout.
	statusProbe func() (*proxyd.DaemonStatusResponse, error)
	// shutdown asks the old daemon to shut down gracefully (force=false).
	shutdown func() error
	// healthAnswers reports whether the old daemon's socket still answers /health.
	healthAnswers func() bool
	// ensureRunning starts/connects a fresh daemon of this process's version.
	ensureRunning func() (*proxyd.Client, error)

	sleep func(time.Duration)
	now   func() time.Time

	drainTimeout time.Duration // max wait for the old socket to stop answering
	drainPoll    time.Duration // interval between /health drain probes
	retryDelay   time.Duration // pause before the one EnsureRunning retry
}

// defaultSkewOps wires skewOps to the real daemon over its Unix socket, with the
// production timeouts.
func defaultSkewOps() skewOps {
	probeClient := proxyd.NewClient(proxyd.SocketPath())
	return skewOps{
		statusProbe: func() (*proxyd.DaemonStatusResponse, error) {
			ctx, cancel := context.WithTimeout(context.Background(), constants.DaemonStatusProbeTimeout)
			defer cancel()
			return probeClient.StatusWithContext(ctx)
		},
		shutdown: func() error { return probeClient.Shutdown(false) },
		healthAnswers: func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), constants.DaemonHealthProbeTimeout)
			defer cancel()
			_, err := probeClient.HealthWithContext(ctx)
			return err == nil
		},
		ensureRunning: proxyd.EnsureRunning,
		sleep:         time.Sleep,
		now:           time.Now,
		drainTimeout:  5 * time.Second,
		drainPoll:     200 * time.Millisecond,
		retryDelay:    1 * time.Second,
	}
}

// recoverFromVersionSkew handles a VersionMismatchError from EnsureRunning per
// D1. It probes the old daemon's /status (short timeout); if the daemon holds no
// projects it auto-heals — asks the old daemon to shut down, waits for its
// socket to drain, then starts a fresh daemon of this version. If the daemon
// holds projects, the probe fails, the shutdown is refused, or the drain/start
// fails, it returns a fatal error with remediation (no standalone fallback).
// Returns the healed client on success.
func recoverFromVersionSkew(vme *proxyd.VersionMismatchError, ops skewOps) (*proxyd.Client, error) {
	status, err := ops.statusProbe()
	if err != nil {
		// Can't tell whether the daemon is busy — never auto-heal blind.
		return nil, versionSkewFatalError(vme, nil)
	}
	if status.ProjectCount > 0 {
		return nil, versionSkewFatalError(vme, status.Routes)
	}

	// Idle daemon: attempt auto-heal. A shutdown refusal (ACTIVE_ROUTES: a
	// project registered in the probe→shutdown TOCTOU window) or any error means
	// it is no longer safe to replace — fall through to the busy-fatal path,
	// re-probing best-effort so the message can name the racing project.
	if serr := ops.shutdown(); serr != nil {
		routes := status.Routes
		if fresh, ferr := ops.statusProbe(); ferr == nil {
			routes = fresh.Routes
		}
		return nil, versionSkewFatalError(vme, routes)
	}

	// Poll until the old daemon's socket stops answering /health (bounded). Never
	// force-kill a still-draining daemon: if it still answers at the deadline,
	// treat it as busy and fail.
	if !waitForDrain(ops.healthAnswers, ops.sleep, ops.now, ops.drainTimeout, ops.drainPoll) {
		return nil, versionSkewFatalError(vme, status.Routes)
	}

	// Start a fresh daemon of this version. The old daemon removes its socket
	// before its deferred PID-lock release, so the first start can lose the race
	// and report not-ready; retry once after a short delay (ErrDaemonNotReady).
	client, err := ensureRunningWithRetry(ops.ensureRunning, ops.sleep, ops.retryDelay)
	if err != nil {
		return nil, versionSkewFatalError(vme, status.Routes)
	}

	fmt.Printf("Replaced idle proxy daemon (version %s) with a fresh daemon (version %s).\n",
		vme.DaemonVersion, vme.ClientVersion)
	return client, nil
}

// ensureRunningWithRetry starts/connects a fresh daemon via ensureRunning,
// retrying once after retryDelay if the first attempt reports
// proxyd.ErrDaemonNotReady. This covers the socket-removed-before-PID-lock-
// released window (an old daemon removes its socket in server.Shutdown before
// its deferred PID-lock release in RunDaemon, so a replacement start can
// briefly lose the lock and report not-ready). Shared by the version-skew
// heal path (D1) and the SHUTTING_DOWN register retry (D4) — both restart a
// daemon that just vacated the same socket/PID-lock pair, so both need the
// identical one-retry guard.
func ensureRunningWithRetry(ensureRunning func() (*proxyd.Client, error), sleep func(time.Duration), retryDelay time.Duration) (*proxyd.Client, error) {
	client, err := ensureRunning()
	if err != nil && errors.Is(err, proxyd.ErrDaemonNotReady) {
		sleep(retryDelay)
		client, err = ensureRunning()
	}
	return client, err
}

// waitForDrain polls healthAnswers until the old daemon's socket stops
// answering /health, bounded by drainTimeout (checked via now, slept between
// probes via drainPoll). Drained requires TWO consecutive misses: a single
// failed probe can be a transient blip on a daemon that is still draining,
// and declaring victory on it would hand the caller a fresh-daemon path that
// immediately collides with the still-live old daemon (burning the one
// re-register attempt). Returns false if the deadline elapsed without a
// confirmed drain — callers must never force-restart over a still-draining
// daemon, so a false means "treat as busy and fail". Shared by the
// version-skew heal path (D1) and the SHUTTING_DOWN register retry (D4),
// which poll the identical old-daemon socket the identical way; only the
// fatal error each builds on a false differs.
func waitForDrain(healthAnswers func() bool, sleep func(time.Duration), now func() time.Time, drainTimeout, drainPoll time.Duration) bool {
	deadline := now().Add(drainTimeout)
	misses := 0
	for {
		if !healthAnswers() {
			misses++
			if misses >= 2 {
				return true
			}
		} else {
			misses = 0
		}
		if !now().Before(deadline) {
			return false
		}
		sleep(drainPoll)
	}
}

// dedupeProjectDirs returns the unique, order-preserving project dirs across the
// given routes. Empty dirs (a very old daemon that omits project_dir) are
// skipped so the caller degrades gracefully to a generic message.
func dedupeProjectDirs(routes []proxyd.RouteInfo) []string {
	seen := make(map[string]struct{}, len(routes))
	var dirs []string
	for _, r := range routes {
		if r.ProjectDir == "" {
			continue
		}
		if _, ok := seen[r.ProjectDir]; ok {
			continue
		}
		seen[r.ProjectDir] = struct{}{}
		dirs = append(dirs, r.ProjectDir)
	}
	return dirs
}

// versionSkewFatalError builds the fatal message for an unrecoverable version
// skew (D1): both versions, the registered project dirs when known, and exact
// remediation. routes may be nil (probe failed or unavailable) — the list is
// omitted gracefully and generic remediation is given.
func versionSkewFatalError(vme *proxyd.VersionMismatchError, routes []proxyd.RouteInfo) error {
	dirs := dedupeProjectDirs(routes)
	var b strings.Builder
	fmt.Fprintf(&b, "shared proxy daemon is running version %s, but this prox is version %s",
		vme.DaemonVersion, vme.ClientVersion)
	if len(dirs) > 0 {
		fmt.Fprintf(&b, "\n\nThe shared daemon still has %d registered project(s):", len(dirs))
		for _, d := range dirs {
			fmt.Fprintf(&b, "\n  - %s", d)
		}
	}
	// Phased deliberately: all the project shutdowns, THEN the daemon, THEN the
	// restarts. Interleaving down/up per project cannot work -- the first
	// project's new-version `prox up` would run while the other old-version
	// projects are still registered, hit the old daemon, and fail this same
	// skew check again. Stopping every project first also removes the need for
	// `prox proxy stop --force` and the ~15s self-heal race that made the old
	// instructions unwinnable (constants.ForwarderHealAfterDown).
	b.WriteString("\n\nTo resolve, stop every affected project, then restart them on this version:")
	if len(dirs) > 0 {
		b.WriteString("\n\n  1. Stop each project:")
		for _, d := range dirs {
			fmt.Fprintf(&b, "\n       (cd %s && prox down)", d)
		}
	} else {
		b.WriteString("\n\n  1. Stop each affected project:  prox down  (run in each project directory)")
	}
	// The daemon schedules its own shutdown shortly after the registry empties
	// (proxyd.Server), so "already gone" is the EXPECTED outcome of step 2, not
	// a failure -- the wording has to say so or the user will think something
	// went wrong.
	b.WriteString("\n  2. Confirm the shared proxy has exited:")
	b.WriteString("\n       prox proxy status     # expect: not running (it may already have exited on its own)")
	b.WriteString("\n     If it is still up:  prox proxy stop")
	if len(dirs) > 0 {
		b.WriteString("\n  3. Restart each project:")
		for _, d := range dirs {
			fmt.Fprintf(&b, "\n       (cd %s && prox up)", d)
		}
	} else {
		b.WriteString("\n  3. Restart each project:  prox up  (run in each project directory)")
	}
	return errors.New(b.String())
}
