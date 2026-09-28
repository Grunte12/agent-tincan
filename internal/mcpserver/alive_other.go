//go:build !unix

package mcpserver

// ProcessAlive cannot check here, so every recorded process counts as
// running.
func ProcessAlive(pid int) bool { return pid > 0 }
