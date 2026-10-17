package client

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"os"
	"os/exec"
)

// PrepareProject invokes only the operator-configured local adapter. No peer
// can select an executable, credential directory, registry, or fallback model.
func (r *Relay) PrepareProject(ctx context.Context, to, project, model string) (string, string, error) {
	if r.sessionScope(ctx) == "" {
		return "", "", errors.New("project communication requires the originating host session ID")
	}
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return "", "", err
	}
	if !caps.SessionRouting {
		return "", "", errors.New("relay does not support session routing")
	}
	agents, err := r.Agents(ctx)
	if err != nil {
		return "", "", err
	}
	kind := ""
	enrolled := false
	for _, agent := range agents {
		if agent.Name == to {
			kind = agent.Kind
			if kind == "proxy-sandbox" {
				kind = "muse"
			}
			enrolled = true
			break
		}
	}
	if !enrolled {
		return "", "", errors.New("project recipient is not enrolled")
	}
	helper := os.Getenv("TINCAN_SESSION_ROUTER")
	unified := helper != "" || caps.SessionCatalog
	if unified {
		// A local operator map covers products upstream enrolls as generic or
		// proxy-sandbox. Peers cannot choose or override an adapter kind.
		var configured map[string]string
		if raw := os.Getenv("TINCAN_SESSION_HOSTS"); raw != "" {
			if json.Unmarshal([]byte(raw), &configured) != nil {
				return "", "", errors.New("invalid operator session host map")
			}
			if configured[to] != "" {
				kind = configured[to]
			}
		}
		if caps.SessionCatalog && os.Getenv("TINCAN_SESSION_CATALOG") != "local" && envelope.ValidSessionHost(kind) {
			binding, err := r.ResolveProjectSession(ctx, to, kind, project)
			if err != nil {
				return "", "", err
			}
			return binding.SessionID, binding.Project, nil
		}
		switch kind {
		case "codex", "cursor", "antigravity", "muse":
		case "claude-code":
			// Relays without the shared catalog keep the local Claude adapter.
			helper = os.Getenv("TINCAN_PROJECT_ADAPTER")
			unified = false
		default:
			return "", "", errors.New("recipient has no supported native session adapter")
		}
	} else {
		if kind != "claude-code" {
			return "", "", errors.New("native project session router is not configured")
		}
		helper = os.Getenv("TINCAN_PROJECT_ADAPTER")
	}
	if helper == "" {
		return "", "", errors.New("project session adapter is not configured by the operator")
	}
	python := os.Getenv("TINCAN_PROJECT_PYTHON")
	if python == "" {
		python = "python3"
	}
	args := []string{helper, "--project", project, "--model", model}
	if unified {
		args = []string{helper, "resolve", "--agent", to, "--host", kind, "--project", project}
	}
	registryEnv := "TINCAN_PROJECT_REGISTRY"
	if unified {
		registryEnv = "TINCAN_SESSION_REGISTRY"
	}
	if registry := os.Getenv(registryEnv); registry != "" {
		args = append(args, "--registry", registry)
	}
	if !unified && os.Getenv("TINCAN_PROJECT_ALLOW_CREATE") == "1" {
		args = append(args, "--allow-create")
	}
	command := exec.CommandContext(ctx, python, args...)
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			switch string(exit.Stderr) {
			case "existing_session_not_discovered\n":
				return "", "", errors.New("existing project conversation has not registered with its native host hook; no task sent")
			case "owner_ambiguous\n":
				return "", "", errors.New("several existing project conversations found without a selected owner; no task sent")
			case "preserved_owner_needs_native_discovery\n":
				return "", "", errors.New("preserved owner awaits native discovery; refusing to substitute a newer chat; no task sent")
			}
		}
		return "", "", errors.New("existing project owner could not be resolved; no task sent (missing, ambiguous or conflicting registry)")
	}
	if len(output) > 65536 {
		return "", "", errors.New("project adapter result too large")
	}
	var result struct {
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
	}
	if json.Unmarshal(output, &result) != nil || result.SessionID == "" || !envelope.ValidSessionID(result.SessionID) || result.Project == "" {
		return "", "", errors.New("invalid project session result")
	}
	return result.SessionID, result.Project, nil
}
