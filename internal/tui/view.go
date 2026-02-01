package tui

import (
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
		return m.BaseModel.mainView(statusInfo)
	}
}

// helpView renders the help overlay
func (m Model) helpView() string {
	help := `
Prox - Process Manager

Navigation:
  j/↓        Scroll down
  k/↑        Scroll up (pauses auto-follow)
  g/Home     Go to top (pauses auto-follow)
  G/End      Go to bottom (resumes auto-follow)
  PgUp/PgDn  Page up/down
  F          Toggle auto-follow mode

Filtering:
  1-9        Solo process (toggle)
  f          Filter mode (process selection)
  /          Pattern filter (regex)
  s          String filter (substring)
  ESC        Clear filters

Other:
  r          Restart selected process (1-9 to select)
  ?          Toggle help
  q/Ctrl+C   Quit

Press any key to close help...
`
	return helpStyle.Render(help)
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
	default:
		return defaultProcessStyle
	}
}
