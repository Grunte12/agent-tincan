package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

func TestClarificationCLIAndWake(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	var hooks hooks
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: hooks.server(t)}}, m.Store,
		wake.Options{ReplyGrace: time.Millisecond, UnseenReplies: m.Server.UnseenReplies, ReplyRetries: []time.Duration{}})
	m.Server.SetEvents(w)
	ctx := t.Context()
	req, err := m.Client(t, "grokbot").Send(ctx, "muse", "private dinner", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Client(t, "muse").Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, replyCmd(), req.ID, "--needs-input", "private question"); err != nil || !strings.Contains(out, "needs_input") {
		t.Fatalf("reply: %s %v", out, err)
	}
	w.Flush()
	if msgs := hooks.got(); len(msgs) != 1 || msgs[0] != wake.WaitingMessage(0, 1) {
		t.Fatalf("wake contains unexpected content: %q", msgs)
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	if out, err := run(t, inboxCmd()); err != nil || !strings.Contains(out, "needs more information") || !strings.Contains(out, "private question") {
		t.Fatalf("inbox: %s %v", out, err)
	}
	if out, err := run(t, answerCmd(), req.ID, "private answer"); err != nil || !strings.Contains(out, "resumed") {
		t.Fatalf("answer: %s %v", out, err)
	}
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, inboxCmd()); err != nil || !strings.Contains(out, "private answer") || !strings.Contains(out, "private dinner") {
		t.Fatalf("resumed inbox: %s %v", out, err)
	}
	if _, err := run(t, replyCmd(), req.ID, "booked"); err != nil {
		t.Fatal(err)
	}
	w.Flush()
}
