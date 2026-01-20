package supervisor

import (
	"os"
	"syscall"
)

// Signal definitions for cross-platform compatibility
var (
	sigterm os.Signal = syscall.SIGTERM
	sigkill os.Signal = syscall.SIGKILL
	sigint  os.Signal = syscall.SIGINT
)
