package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestApprovalCommands(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	path := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(path, []byte(`{"gate":{"muse":{"from":"*"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := policy.LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Server.SetPreparer(policy.New(m.Store, policy.Config{Approval: a}))
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	out, err := run(t, askCmd(), "muse", "please call", "--wait", "0s")
	if err != nil || !strings.Contains(out, "waiting for the owner's approval") {
		t.Fatalf("ask: %q %v", out, err)
	}
	held, err := m.Store.Held(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatalf("held: %+v %v", held, err)
	}
	id := held[0].ID
	out, err = run(t, heldCmd(), "--relay", m.URL("admin"))
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "grokbot -> muse") || !strings.Contains(out, "please call") {
		t.Fatalf("list: %q %v", out, err)
	}
	socket := adminSocket(t, m)
	if _, err := run(t, approvalCmd("approve"), id, "--socket", socket); err != nil {
		t.Fatal(err)
	}
	res, err := m.Client(t, "grokbot").Get(t.Context(), id, 0)
	if err != nil || res.Status != envelope.StatusQueued {
		t.Fatalf("approved: %+v %v", res, err)
	}
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "another", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, approvalCmd("deny"), req.ID, "not", "today", "--relay", m.URL("admin")); err != nil {
		t.Fatal(err)
	}
	res, err = m.Client(t, "grokbot").Get(t.Context(), req.ID, 0)
	if err != nil || res.Reply == nil || res.Reply.Body != "not today" {
		t.Fatalf("denied: %+v %v", res, err)
	}
	if _, err := run(t, heldCmd(), "--relay", m.URL("grokbot")); !client.IsStatus(err, 403) {
		t.Fatalf("agent list: %v", err)
	}
}
