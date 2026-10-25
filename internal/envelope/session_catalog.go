package envelope

import (
	"path/filepath"
	"strings"
)

// SessionBinding is communication metadata only. Agent and session are set
// from the authenticated native caller; peers never choose them in discovery.
type SessionBinding struct {
	Agent     string `json:"agent"`
	Host      string `json:"host"`
	Project   string `json:"project"`
	SessionID string `json:"session_id"`
}

func ValidSessionHost(host string) bool {
	switch host {
	case "codex", "cursor", "antigravity", "muse", "claude-code", "opencode", "hermes":
		return true
	}
	return false
}

// ValidSessionProject validates a canonical project key at both HTTP and store boundaries.
func ValidSessionProject(project string) bool {
	return project != "" && len(project) <= 4096 && strings.IndexByte(project, 0) < 0 && filepath.IsAbs(project) && filepath.Clean(project) == project
}
