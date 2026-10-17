//go:build !(darwin || linux || freebsd || openbsd || netbsd || dragonfly)

package mcpserver

import "errors"

// Legacy/native host transports still work on other platforms. The optional
// private POSIX hook adapter fails closed until secure key storage is available.
func readHostKey(string) ([]byte, error) {
	return nil, errors.New("private native hook context is unavailable on this platform")
}
