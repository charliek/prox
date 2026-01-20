package logs

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charliek/prox/internal/domain"
)

// MaxPatternLength is the maximum allowed length for filter patterns
// to prevent potential DoS attacks from excessively complex patterns
const MaxPatternLength = 256

// Filter applies a LogFilter to log entries
type Filter struct {
	filter domain.LogFilter
	regex  *regexp.Regexp
}

// NewFilter creates a new filter from a LogFilter
func NewFilter(filter domain.LogFilter) (*Filter, error) {
	f := &Filter{filter: filter}

	// Validate pattern length to prevent DoS
	if len(filter.Pattern) > MaxPatternLength {
		return nil, fmt.Errorf("%w: pattern exceeds maximum length of %d characters", domain.ErrInvalidPattern, MaxPatternLength)
	}

	if filter.Pattern != "" && filter.IsRegex {
		re, err := regexp.Compile(filter.Pattern)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrInvalidPattern, err)
		}
		f.regex = re
	}

	return f, nil
}

// Matches returns true if the entry matches the filter criteria
func (f *Filter) Matches(entry domain.LogEntry) bool {
	// Check process filter
	if !f.filter.MatchesProcess(entry.Process) {
		return false
	}

	// Check pattern filter
	if f.filter.Pattern != "" {
		if f.regex != nil {
			if !f.regex.MatchString(entry.Line) {
				return false
			}
		} else {
			if !strings.Contains(entry.Line, f.filter.Pattern) {
				return false
			}
		}
	}

	return true
}

// FilterEntries filters a slice of log entries
func FilterEntries(entries []domain.LogEntry, filter domain.LogFilter) ([]domain.LogEntry, error) {
	if filter.IsEmpty() {
		return entries, nil
	}

	f, err := NewFilter(filter)
	if err != nil {
		return nil, err
	}

	result := make([]domain.LogEntry, 0, len(entries))
	for _, entry := range entries {
		if f.Matches(entry) {
			result = append(result, entry)
		}
	}

	return result, nil
}

// FilterEntriesLimit filters entries and returns at most limit entries
func FilterEntriesLimit(entries []domain.LogEntry, filter domain.LogFilter, limit int) ([]domain.LogEntry, int, error) {
	filtered, err := FilterEntries(entries, filter)
	if err != nil {
		return nil, 0, err
	}

	total := len(filtered)
	if limit > 0 && len(filtered) > limit {
		// Return last 'limit' entries
		filtered = filtered[len(filtered)-limit:]
	}

	return filtered, total, nil
}
