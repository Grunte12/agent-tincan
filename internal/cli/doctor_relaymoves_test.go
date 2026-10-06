package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// An agent that cannot list its tailnet is warned that it would never find
// a moved relay, and told how to fix it; one that can is told how.
func TestRelayMovesCheck(t *testing.T) {
	none := func(context.Context) ([]string, error) { return nil, errors.New("no tailscaled") }
	restore := client.SwapNetmapLookups(none, none)
	c := relayMovesCheck(t.Context())
	restore()
	if c.Status != "warn" || !strings.Contains(c.Detail, "cannot list its tailnet") || !strings.Contains(c.Fix, "TS_SOCKET") {
		t.Fatalf("no tailnet listing: %+v", c)
	}

	some := func(context.Context) ([]string, error) { return []string{"100.64.0.1", "100.64.0.2"}, nil }
	t.Cleanup(client.SwapNetmapLookups(some, none))
	c = relayMovesCheck(t.Context())
	if c.Status != "ok" || !strings.Contains(c.Detail, "lists 2 tailnet addresses through tailscaled's LocalAPI") {
		t.Fatalf("tailnet listed: %+v", c)
	}
}
