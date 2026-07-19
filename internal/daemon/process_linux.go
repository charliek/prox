//go:build linux

package daemon

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// ProcessStartTime returns an opaque, host-and-boot-local generation token for
// pid (Linux: /proc/<pid>/stat field 22, starttime in clock ticks since boot).
// ok=false when unreadable. See IsProcessAlive for how it is used.
func ProcessStartTime(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	return parseProcStatStartTime(data)
}

// parseProcStatStartTime extracts stat field 22 (starttime). comm (field 2) is
// parenthesized and may itself contain spaces and ')'; parse AFTER the LAST ')'
// so it can't shift the fields. In that remainder the tokens start at field 3
// (state), so starttime (field 22) is index 19.
func parseProcStatStartTime(data []byte) (int64, bool) {
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	const startTimeIdx = 19 // stat field 22 == index 19 after the trailing ')'
	if len(fields) <= startTimeIdx {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[startTimeIdx], 10, 64)
	if err != nil || v == 0 || v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}
