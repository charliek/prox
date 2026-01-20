package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
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
		{"unknown error", errors.New("some error"), "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ErrorCode(tt.err))
		})
	}
}
