package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/charliek/prox/internal/api"
	"github.com/charliek/prox/internal/constants"
	"github.com/charliek/prox/internal/domain"
)

// LogPrinter handles consistent log formatting and color assignment
type LogPrinter struct {
	colors     map[string]string
	colorIndex int
}

// NewLogPrinter creates a new LogPrinter
func NewLogPrinter() *LogPrinter {
	return &LogPrinter{
		colors: make(map[string]string),
	}
}

// PrintEntry prints a log entry with consistent color assignment
func (lp *LogPrinter) PrintEntry(entry domain.LogEntry) {
	ts := entry.Timestamp.Format("15:04:05")
	if lp.isTerminal() {
		color := lp.getColor(entry.Process)
		fmt.Printf("%s %s%-8s%s | %s\n", ts, color, entry.Process, constants.ColorReset, entry.Line)
	} else {
		fmt.Printf("%s %-8s | %s\n", ts, entry.Process, entry.Line)
	}
}

// PrintAPIEntry prints an API log entry response
func (lp *LogPrinter) PrintAPIEntry(entry api.LogEntryResponse) {
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		ts = time.Now()
	}
	if lp.isTerminal() {
		color := lp.getColor(entry.Process)
		fmt.Printf("%s %s%-8s%s | %s\n", ts.Format("15:04:05"), color, entry.Process, constants.ColorReset, entry.Line)
	} else {
		fmt.Printf("%s %-8s | %s\n", ts.Format("15:04:05"), entry.Process, entry.Line)
	}
}

func (lp *LogPrinter) getColor(process string) string {
	color, ok := lp.colors[process]
	if !ok {
		color = constants.ProcessColors[lp.colorIndex%len(constants.ProcessColors)]
		lp.colors[process] = color
		lp.colorIndex++
	}
	return color
}

// isTerminal returns true if stdout is connected to a terminal.
func (lp *LogPrinter) isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
