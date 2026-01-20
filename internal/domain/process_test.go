package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestProcessState_String(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  string
	}{
		{ProcessStateRunning, "running"},
		{ProcessStateStopped, "stopped"},
		{ProcessStateStarting, "starting"},
		{ProcessStateStopping, "stopping"},
		{ProcessStateCrashed, "crashed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.String())
		})
	}
}

func TestProcessState_IsRunning(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  bool
	}{
		{ProcessStateRunning, true},
		{ProcessStateStopped, false},
		{ProcessStateStarting, false},
		{ProcessStateStopping, false},
		{ProcessStateCrashed, false},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.IsRunning())
		})
	}
}

func TestProcessState_IsStopped(t *testing.T) {
	tests := []struct {
		state ProcessState
		want  bool
	}{
		{ProcessStateRunning, false},
		{ProcessStateStopped, true},
		{ProcessStateStarting, false},
		{ProcessStateStopping, false},
		{ProcessStateCrashed, true},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.state.IsStopped())
		})
	}
}

func TestProcessInfo_UptimeSeconds(t *testing.T) {
	t.Run("zero when not started", func(t *testing.T) {
		info := ProcessInfo{}
		assert.Equal(t, int64(0), info.UptimeSeconds())
	})

	t.Run("calculates uptime", func(t *testing.T) {
		info := ProcessInfo{
			StartedAt: time.Now().Add(-10 * time.Second),
		}
		uptime := info.UptimeSeconds()
		assert.GreaterOrEqual(t, uptime, int64(9))
		assert.LessOrEqual(t, uptime, int64(11))
	})
}
