package proxy

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrPortInUse is returned when a proxy port is already bound by another process.
var ErrPortInUse = errors.New("port already in use")

// PortConflictError carries metadata about which port and protocol conflicted.
// It wraps both the ErrPortInUse sentinel and the original OS error so that
// errors.Is works for either (Go 1.20+ multi-unwrap).
type PortConflictError struct {
	Port     int
	Protocol string // "HTTP" or "HTTPS"
	Cause    error  // original net.Listen error
}

func (e *PortConflictError) Error() string {
	return fmt.Sprintf("%s proxy port %d already in use", e.Protocol, e.Port)
}

func (e *PortConflictError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrPortInUse, e.Cause}
	}
	return []error{ErrPortInUse}
}

// isAddrInUse checks whether an error from net.Listen is caused by the address
// already being in use (EADDRINUSE). On Go 1.13+ the net and os packages
// implement Unwrap(), so errors.Is traverses the chain correctly.
func isAddrInUse(err error) bool {
	return err != nil && errors.Is(err, syscall.EADDRINUSE)
}
