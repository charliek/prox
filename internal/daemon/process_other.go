//go:build !darwin && !linux

package daemon

// ProcessStartTime has no portable implementation on this platform; returning
// ok=false makes IsProcessAlive fall back to bare-PID liveness.
func ProcessStartTime(pid int) (int64, bool) { return 0, false }
