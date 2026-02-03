package cli

import (
	"fmt"
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
	color := lp.getColor(entry.Process)
	ts := entry.Timestamp.Format("15:04:05")
	fmt.Printf("%s %s%-8s%s | %s\n", ts, color, entry.Process, constants.ColorReset, entry.Line)
}

// PrintAPIEntry prints an API log entry response
func (lp *LogPrinter) PrintAPIEntry(entry api.LogEntryResponse) {
	color := lp.getColor(entry.Process)
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		ts = time.Now()
	}
	fmt.Printf("%s %s%-8s%s | %s\n", ts.Format("15:04:05"), color, entry.Process, constants.ColorReset, entry.Line)
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
