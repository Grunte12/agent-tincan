package mcpserver_test

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"strings"
	"sync"
	"testing"
)

func TestCodexHostMetadataRoutesConcurrentCallsWithoutStartupScope(t *testing.T) {
	t.Setenv("TINCAN_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	m := testrelay.New(t, relay.Config{})
	owner, err := client.NewRelayForFile(client.Config{Relay: m.URL("grokbot"), Agent: "grokbot", SessionID: "imem-owner"}, "")
	if err != nil {
		t.Fatal(err)
	}
	req, err := owner.Send(t.Context(), "muse", "synthetic receipt", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	worker := m.Client(t, "muse")
	if _, err = worker.Poll(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Claim(t.Context(), req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.Reply(t.Context(), req.ID, "synthetic complete", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := mcpserver.New(m.Client(t, "grokbot"), "test").Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "codex-mcp-client", Version: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	run := func(ctx context.Context, id string) (bool, error) {
		var meta mcp.Meta
		if id != "" {
			meta = mcp.Meta{"x-codex-turn-metadata": map[string]any{"thread_id": id, "session_id": "runtime-not-chat"}}
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_reply", Arguments: map[string]any{"request_id": req.ID, "wait_seconds": 1}, Meta: meta})
		if err != nil {
			return false, err
		}
		return res.IsError, nil
	}
	var wg sync.WaitGroup
	for _, id := range []string{"imem-owner", "plugin-chat", "imem-owner", "plugin-chat", ""} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			failed, err := run(t.Context(), id)
			if err != nil || failed != (id != "imem-owner") {
				t.Errorf("scope=%q failed=%v error=%v", id, failed, err)
			}
		}(id)
	}
	wg.Wait()
	// A subsequent call without metadata must not inherit the last chat's scope.
	failed, err := run(t.Context(), "")
	if err != nil || !failed {
		t.Fatalf("unscoped call leaked owner: %v %v", failed, err)
	}
	bad, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}, Meta: mcp.Meta{"x-codex-turn-metadata": map[string]any{"thread_id": "bad/id"}}})
	if err != nil || !bad.IsError {
		t.Fatalf("invalid host ID accepted: %+v %v", bad, err)
	}
	raw, _ := bad.Content[0].(*mcp.TextContent)
	if raw == nil || !strings.Contains(raw.Text, "thread_id") {
		t.Fatal("missing validation error")
	}
}
