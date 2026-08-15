//go:build unix

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinInForeground reports whether the terminal on stdin has THIS process's
// group as its foreground process group.
//
// This is the `prox up &` check (condition 3 of terminalHostable). A
// backgrounded job keeps the terminal on all three fds, so isatty and TERM both
// say "go ahead"; the failure only appears when bubbletea reads stdin, at which
// point the kernel raises SIGTTIN and the shell stops the job — a hang with no
// diagnostic.
//
// Any ioctl error means "not in the foreground". That is the conservative
// answer: an fd we cannot interrogate (a pipe, a closed descriptor, a platform
// that answers ENOTTY) is precisely one we should not hand to a TUI. Note that
// condition 1 has already established stdin is a terminal by the time this
// runs, so an error here is genuinely unexpected rather than the common case.
func stdinInForeground() bool {
	fg, err := unix.IoctlGetInt(int(os.Stdin.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return false
	}
	return fg == unix.Getpgrp()
}
