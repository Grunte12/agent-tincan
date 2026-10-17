package envelope

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
	case "codex", "cursor", "antigravity", "muse", "claude-code":
		return true
	}
	return false
}
