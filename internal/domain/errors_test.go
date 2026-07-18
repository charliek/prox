package domain

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"process not found", ErrProcessNotFound, ErrCodeProcessNotFound},
		{"process already running", ErrProcessAlreadyRunning, ErrCodeProcessAlreadyRunning},
		{"process not running", ErrProcessNotRunning, ErrCodeProcessNotRunning},
		{"invalid pattern", ErrInvalidPattern, ErrCodeInvalidPattern},
		{"shutdown in progress", ErrShutdownInProgress, ErrCodeShutdownInProgress},
		{"env reload failed", ErrEnvReloadFailed, ErrCodeEnvReloadFailed},
		{"process group not reaped", ErrProcessGroupNotReaped, ErrCodeProcessGroupNotReaped},
		{"unknown error", errors.New("some error"), "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ErrorCode(tt.err))
		})
	}
}

func TestProcessStopError_IsAndAs(t *testing.T) {
	err := &ProcessStopError{Failures: []ProcessStopFailure{
		{Name: "web", Err: fmt.Errorf("%w: web", ErrProcessGroupNotReaped)},
	}}

	// errors.Is sees through the aggregate to the wrapped sentinel.
	assert.ErrorIs(t, error(err), ErrProcessGroupNotReaped)

	// errors.As extracts the typed aggregate for serialization.
	var got *ProcessStopError
	require.ErrorAs(t, error(err), &got)
	assert.Same(t, err, got)
}

func TestProcessStopError_Message(t *testing.T) {
	single := &ProcessStopError{Failures: []ProcessStopFailure{
		{Name: "web", Err: ErrProcessGroupNotReaped},
	}}
	assert.Contains(t, single.Error(), "web")
	assert.Contains(t, single.Error(), ErrProcessGroupNotReaped.Error())

	multi := &ProcessStopError{Failures: []ProcessStopFailure{
		{Name: "web", Err: ErrProcessGroupNotReaped},
		{Name: "worker", Err: ErrProcessGroupNotReaped},
	}}
	msg := multi.Error()
	assert.Contains(t, msg, "web")
	assert.Contains(t, msg, "worker")
	assert.Contains(t, msg, "2 processes")
}

func TestProcessStopError_UnwrapMulti(t *testing.T) {
	e1 := fmt.Errorf("%w: a", ErrProcessGroupNotReaped)
	e2 := errors.New("ctx canceled")
	err := &ProcessStopError{Failures: []ProcessStopFailure{{Name: "a", Err: e1}, {Name: "b", Err: e2}}}

	unwrapped := err.Unwrap()
	require.Len(t, unwrapped, 2)
	assert.Same(t, e1, unwrapped[0])
	assert.Same(t, e2, unwrapped[1])
}
