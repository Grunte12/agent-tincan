package mcpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// A local native hook signs the actual conversation ID. The model cannot
// choose an ID, change the roster identity or reuse another host's context.
// This is a local MCP adapter only; native relay envelopes and leases stay
// unchanged, and neither tokens nor hook payloads are forwarded to the relay.
type hostClaims struct {
	Version   int    `json:"version"`
	Agent     string `json:"agent"`
	Host      string `json:"host"`
	Project   string `json:"project"`
	SessionID string `json:"session_id"`
	Expires   int64  `json:"expires"`
}

func hostContextSession(token, keyPath, agent, host string, now time.Time) (string, error) {
	invalid := errors.New("valid native host_context required; run the existing conversation's routing hook, never guess a session ID")
	if len(token) > 8192 || keyPath == "" || !filepath.IsAbs(keyPath) {
		return "", invalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", invalid
	}
	key, err := readHostKey(keyPath)
	if err != nil {
		return "", invalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return "", invalid
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", invalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return "", invalid
	}
	var claim hostClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claim) != nil || claim.Version != 1 || claim.Agent != agent || agent == "" || claim.Host != host || claim.Expires <= now.Unix() || claim.Expires > now.Add(24*time.Hour+time.Minute).Unix() || claim.SessionID == "" || !envelope.ValidSessionID(claim.SessionID) || !filepath.IsAbs(claim.Project) || filepath.Clean(claim.Project) != claim.Project {
		return "", invalid
	}
	return claim.SessionID, nil
}
