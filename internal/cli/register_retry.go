package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/proxyd"
)

// registerRetryOps bundles the injectable operations the SHUTTING_DOWN
// register retry (D4) needs, mirroring skewOps (version_skew.go) so
// retryRegisterAfterShutdown can be unit-tested against a fake daemon without
// real sockets or wall-clock waits. defaultRegisterRetryOps wires the
// production implementations.
type registerRetryOps struct {
	// healthAnswers reports whether the (old, shutting-down) daemon's socket
	// still answers /health.
	healthAnswers func() bool
	// ensureRunning starts/connects a fresh daemon of this process's version.
	ensureRunning func() (*proxyd.Client, error)

	sleep func(time.Duration)
	now   func() time.Time

	drainTimeout time.Duration // max wait for the old socket to stop answering
	drainPoll    time.Duration // interval between /health drain probes
	retryDelay   time.Duration // pause before the one EnsureRunning retry
}

// defaultRegisterRetryOps wires registerRetryOps to the real daemon over its
// Unix socket, with the production timings from D4 (§3): a 10s bound on the
// /health drain poll at a 200ms interval, matching the daemon's 5s
// empty-shutdown grace with headroom.
func defaultRegisterRetryOps() registerRetryOps {
	probeClient := proxyd.NewClient(proxyd.SocketPath())
	return registerRetryOps{
		healthAnswers: func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), constants.DaemonHealthProbeTimeout)
			defer cancel()
			_, err := probeClient.HealthWithContext(ctx)
			return err == nil
		},
		ensureRunning: proxyd.EnsureRunning,
		sleep:         time.Sleep,
		now:           time.Now,
		drainTimeout:  10 * time.Second,
		drainPoll:     200 * time.Millisecond,
		retryDelay:    1 * time.Second,
	}
}

// isShuttingDownError reports whether err is the daemon's SHUTTING_DOWN
// register response (503, ErrorResponse.Code == "SHUTTING_DOWN"): the
// register queued behind the daemon's graceful-shutdown decision
// (server.go's isShuttingDown check) rather than a genuine failure.
func isShuttingDownError(err error) bool {
	var apiErr *proxyd.DaemonAPIError
	return errors.As(err, &apiErr) && apiErr.Code == "SHUTTING_DOWN"
}

// isVersionMismatchError reports whether err is the daemon's VERSION_MISMATCH
// register response (409, ErrorResponse.Code == "VERSION_MISMATCH"): a busy
// different-version daemon holds the ports and re-checked versions at register
// time. Mirrors isShuttingDownError; the heal path (D6b) uses it to set
// heal_state version_mismatch rather than healing (FIX 5), matching the
// EnsureRunning VersionMismatchError treatment.
func isVersionMismatchError(err error) bool {
	var apiErr *proxyd.DaemonAPIError
	return errors.As(err, &apiErr) && apiErr.Code == "VERSION_MISMATCH"
}

// retryRegisterAfterShutdown implements D4: when Register fails with the
// daemon's SHUTTING_DOWN code, the daemon is mid graceful-shutdown (its 5s
// empty-shutdown grace — server.go), not genuinely refusing the project. It
// polls until the old daemon's socket stops answering /health (bounded by
// ops.drainTimeout), starts a fresh daemon of this version — reusing
// ensureRunningWithRetry, the same one-retry-on-ErrDaemonNotReady guard the
// version-skew heal path uses for the identical socket-removed-before-PID-
// lock-released window — and re-registers once against it.
//
// A second SHUTTING_DOWN or any other error from either the restart or the
// re-register is returned as-is: the caller treats it as fatal exactly as an
// unrecovered register failure is today (one retry layer only).
func retryRegisterAfterShutdown(req proxyd.RegisterRequest, ops registerRetryOps) (*proxyd.Client, *proxyd.RegisterResponse, error) {
	if !waitForDrain(ops.healthAnswers, ops.sleep, ops.now, ops.drainTimeout, ops.drainPoll) {
		return nil, nil, fmt.Errorf("proxy daemon is shutting down and did not finish within %s", ops.drainTimeout)
	}

	client, err := ensureRunningWithRetry(ops.ensureRunning, ops.sleep, ops.retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("starting fresh proxy daemon after shutdown: %w", err)
	}

	resp, err := client.Register(req)
	if err != nil {
		return nil, nil, err
	}

	fmt.Println("proxy daemon was shutting down; started a fresh one")
	return client, resp, nil
}
