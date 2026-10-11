package cli

import (
	"encoding/json"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// tincan agents --json prints the relay's roster document, node fields and
// all when the caller is an admin device.
func TestAgentsJSONPrintsRoster(t *testing.T) {
	m := testrelay.New(t, relay.Config{Version: "v9.9.9"})
	useConfig(t, client.Config{})
	stdout, _, err := runSplit(t, Root(), "agents", "--json", "--relay", m.URL("admin"))
	if err != nil {
		t.Fatalf("agents --json: %v", err)
	}
	var ro client.Roster
	if err := json.Unmarshal([]byte(stdout), &ro); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if ro.RelayVersion != "v9.9.9" || len(ro.Agents) != 3 {
		t.Fatalf("roster = %+v", ro)
	}
	for _, a := range ro.Agents {
		if a.Name == "grokbot" && (a.NodeID != "nGROK" || a.NodeName != "grok-bot" || a.NodeUser != testrelay.Login) {
			t.Errorf("grokbot = %+v, want its node", a)
		}
	}
}
