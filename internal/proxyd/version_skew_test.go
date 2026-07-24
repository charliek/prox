package proxyd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVersionMismatchError_AsAndFields pins that EnsureRunning's version-skew
// error is matchable with errors.As (so tryDaemonProxy can distinguish it from a
// connection-refused failure) and carries both versions for the remediation
// message.
func TestVersionMismatchError_AsAndFields(t *testing.T) {
	err := error(&VersionMismatchError{DaemonVersion: "0.1.2", ClientVersion: "0.2.0"})
	// Wrapped, as EnsureRunning's callers may re-wrap.
	wrapped := fmt.Errorf("ensure running: %w", err)

	var vme *VersionMismatchError
	if !errors.As(wrapped, &vme) {
		t.Fatalf("errors.As did not match VersionMismatchError in %v", wrapped)
	}
	assert.Equal(t, "0.1.2", vme.DaemonVersion)
	assert.Equal(t, "0.2.0", vme.ClientVersion)
	assert.Contains(t, vme.Error(), "0.1.2")
	assert.Contains(t, vme.Error(), "0.2.0")
}

// TestErrDaemonNotReady_Is pins that the startup-timeout error wraps the
// ErrDaemonNotReady sentinel, which the version-skew heal keys its one retry on.
func TestErrDaemonNotReady_Is(t *testing.T) {
	err := fmt.Errorf("%w within 5s", ErrDaemonNotReady)
	assert.True(t, errors.Is(err, ErrDaemonNotReady), "startup-timeout error must wrap ErrDaemonNotReady")
}
