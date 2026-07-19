//go:build darwin

package daemon

import "golang.org/x/sys/unix"

// ProcessStartTime returns an opaque, host-and-boot-local generation token for
// pid (Darwin: kinfo_proc P_starttime, process creation time in microseconds).
// ok=false when unreadable. See IsProcessAlive for how it is used.
func ProcessStartTime(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, false
	}
	// Defend against a stale/empty kinfo naming a different process.
	if kp.Proc.P_pid != int32(pid) {
		return 0, false
	}
	sec := int64(kp.Proc.P_starttime.Sec)
	usec := int64(kp.Proc.P_starttime.Usec)
	if sec <= 0 || usec < 0 || usec >= 1_000_000 {
		return 0, false
	}
	token := sec*1_000_000 + usec
	if token <= 0 {
		return 0, false
	}
	return token, true
}
