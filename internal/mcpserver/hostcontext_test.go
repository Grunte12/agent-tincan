package mcpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func signHostContext(t *testing.T, keyPath string, claim hostClaims) string {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	dir := filepath.Dir(keyPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestHostContextValidation(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "host.key")
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	base := hostClaims{Version: 1, Agent: "grokbot", Host: "cursor", Project: "/tmp/project", SessionID: "chat-a", Expires: now.Add(time.Hour).Unix()}
	valid := signHostContext(t, keyPath, base)
	tampered := valid[:len(valid)-1] + "A"
	if tampered == valid {
		tampered = valid[:len(valid)-1] + "B"
	}
	otherDir := filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0700); err != nil {
		t.Fatal(err)
	}
	otherKey := filepath.Join(otherDir, "host.key")
	if err := os.WriteFile(otherKey, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	badMode := filepath.Join(dir, "bad-mode.key")
	if err := os.WriteFile(badMode, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badMode, 0644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "linked.key")
	if err := os.Symlink(keyPath, symlink); err != nil {
		t.Fatal(err)
	}
	keyAsDirectory := filepath.Join(dir, "key-directory")
	if err := os.Mkdir(keyAsDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if got, err := hostContextSession(valid, keyPath, "grokbot", "cursor", now); err != nil || got != "chat-a" {
		t.Fatalf("valid token: got %q, err %v", got, err)
	}
	for name, tc := range map[string]struct {
		token, keyPath, agent, host string
		at                          time.Time
	}{
		"missing":              {"", keyPath, "grokbot", "cursor", now},
		"expired":              {signHostContext(t, keyPath, func() hostClaims { c := base; c.Expires = now.Unix(); return c }()), keyPath, "grokbot", "cursor", now},
		"cross-host":           {valid, keyPath, "grokbot", "muse", now},
		"other-agent":          {valid, keyPath, "muse", "cursor", now},
		"wrong-key":            {valid, otherKey, "grokbot", "cursor", now},
		"invalid-key-mode":     {valid, badMode, "grokbot", "cursor", now},
		"key-symlink":          {valid, symlink, "grokbot", "cursor", now},
		"key-not-regular-file": {valid, keyAsDirectory, "grokbot", "cursor", now},
		"tampered":             {tampered, keyPath, "grokbot", "cursor", now},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hostContextSession(tc.token, tc.keyPath, tc.agent, tc.host, tc.at); err == nil {
				t.Fatal("invalid native scope accepted")
			}
		})
	}
}
