//go:build !unix

package cli

// stdinInForeground is the non-unix fallback for terminalHostable's third
// condition. Job control and TIOCGPGRP are a POSIX concept; on platforms
// without them there is no background-job hazard of this shape to detect, so
// the check passes and hostability rests on conditions 1 and 2 alone.
//
// Releases only target linux and darwin (.goreleaser.yaml) and internal/daemon
// is unix-only anyway, so nothing currently reaches this file — it exists so
// that terminalHostable stays portable by construction rather than by accident,
// mirroring internal/daemon/process_other.go.
func stdinInForeground() bool { return true }
