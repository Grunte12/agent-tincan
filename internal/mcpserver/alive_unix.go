//go:build unix

package mcpserver

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LaunchRunning reports whether the tincan mcp that wrote l is still
// running. A record whose process was killed before it could note its end
// keeps its pid, and the system can hand that pid to an unrelated process
// later, so a live pid counts only if its process started no later than the
// record (which tincan mcp writes as it starts).
func LaunchRunning(l Launch) bool {
	if !l.Ended.IsZero() || l.PID <= 0 {
		return false
	}
	if err := syscall.Kill(l.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	start, ok := processStart(l.PID)
	if !ok || l.Started.IsZero() {
		return true // cannot tell; the pid is alive
	}
	// ps reports whole seconds.
	return !start.After(l.Started.Add(2 * time.Second))
}

// processStart is when process pid started, from ps (the same format on
// Linux and macOS).
func processStart(pid int) (time.Time, bool) {
	c := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	c.Env = append(os.Environ(), "LC_ALL=C")
	out, err := c.Output()
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "), time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
