package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"os/exec"
	"path/filepath"
	"time"
)

// CommandSessionCreator runs a fixed operator-owned executable, never a shell
// or a peer-selected command. Its stdout must contain only the native binding.
func CommandSessionCreator(command string) func(context.Context, string, string, string) (envelope.SessionBinding, error) {
	if command == "" {
		return nil
	}
	return func(ctx context.Context, agent, host, project string) (envelope.SessionBinding, error) {
		var binding envelope.SessionBinding
		if !filepath.IsAbs(command) {
			return binding, errors.New("operator session creator must be absolute")
		}
		data, _ := json.Marshal(map[string]string{"agent": agent, "host": host, "project": project})
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, command)
		cmd.Stdin = bytes.NewReader(data)
		// Do not relay native stderr: it may contain credentials or unrelated output.
		out, err := cmd.Output()
		if err != nil {
			return binding, errors.New("native project session creation failed or is unconfirmed; no task sent; inspect operator creation receipt before retry")
		}
		if len(out) > 8192 || json.Unmarshal(out, &binding) != nil {
			return binding, errors.New("invalid native creation binding")
		}
		return binding, nil
	}
}
