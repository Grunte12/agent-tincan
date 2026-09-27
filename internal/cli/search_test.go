package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestSearchCLI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	req := answered(t, m, "restaurant booking", "confirmed Tuesday")
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, Root(), "search", "restaurant", "--limit", "1")
	if err != nil || !strings.Contains(out, req.TraceID) || !strings.Contains(out, "[restaurant]") {
		t.Fatalf("search = %s, %v", out, err)
	}
	out, err = run(t, Root(), "search", "Tuesday", "--json")
	var hits []envelope.SearchResult
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RequestID != req.ID {
		t.Fatalf("JSON = %s", out)
	}
	out, err = run(t, Root(), "search", "missing", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty JSON = %q, %v", out, err)
	}
	for _, limit := range []string{"0", "51"} {
		if _, err := run(t, Root(), "search", "restaurant", "--limit", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
}
