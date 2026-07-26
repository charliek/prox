package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/prox/internal/domain"
)

// View renders the TUI
func (m Model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	switch m.mode {
	case ModeHelp:
		return m.helpView()
	default:
		statusInfo := ""
		if m.lastRestartProcess != "" {
			if m.lastRestartError != nil {
				statusInfo = "Restart failed: " + truncateError(m.lastRestartError, maxErrorDisplayLen)
			} else {
				statusInfo = "Restarted: " + m.lastRestartProcess
			}
		}
		return m.mainView(statusInfo)
	}
}

// getProcessStyle returns the style for a process name
func getProcessStyle(name string, processes []domain.ProcessInfo) lipgloss.Style {
	// Find process index for color
	for i, p := range processes {
		if p.Name == name {
			return processColors[i%len(processColors)]
		}
	}
	return defaultProcessStyle
}

// processStyle returns style based on process state
func processStyle(state domain.ProcessState) lipgloss.Style {
	switch state {
	case domain.ProcessStateRunning:
		return runningStyle
	case domain.ProcessStateStopped:
		return stoppedStyle
	case domain.ProcessStateCrashed:
		return crashedStyle
	case domain.ProcessStateStarting:
		return startingStyle
	case domain.ProcessStateStopping:
		return stoppingStyle
	case domain.ProcessStateWaiting:
		return waitingStyle
	case domain.ProcessStateBlocked:
		return blockedStyle
	case domain.ProcessStateCompleted:
		return completedStyle
	default:
		return defaultProcessStyle
	}
}

// gatedDetail returns the inline gated-launch annotation for a process (plan 013
// D5): " (waiting on: X, Y)" while waiting, " (blocked on: X)" while blocked, and
// "" in every other state. Targets are shown in declaration order.
func gatedDetail(p domain.ProcessInfo) string {
	switch p.State {
	case domain.ProcessStateWaiting:
		if len(p.WaitingOn) > 0 {
			return " (waiting on: " + strings.Join(p.WaitingOn, ", ") + ")"
		}
	case domain.ProcessStateBlocked:
		if len(p.BlockedOn) > 0 {
			return " (blocked on: " + strings.Join(p.BlockedOn, ", ") + ")"
		}
	}
	return ""
}
