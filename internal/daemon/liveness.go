package daemon

// IsProcessAlive reports whether pid still names the same process generation
// that recorded startTime. Semantics are "process identity still exists," not
// "running": like ProcessExists it treats a zombie as existing (reaping zombies
// is a non-goal). Fallbacks bias toward "alive" so a live process is never
// falsely reaped:
//
//	startTime == 0          -> bare ProcessExists (no token captured)
//	current token unreadable -> bare ProcessExists (can't disprove identity)
func IsProcessAlive(pid int, startTime int64) bool {
	if !ProcessExists(pid) {
		return false
	}
	if startTime == 0 {
		return true
	}
	cur, ok := ProcessStartTime(pid)
	if !ok {
		return true
	}
	return cur == startTime
}
