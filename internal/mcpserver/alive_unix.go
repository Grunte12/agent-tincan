//go:build unix

package mcpserver

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether process pid still exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
