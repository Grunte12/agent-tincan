package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestClarificationOverMCP(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	asker, handler := session(t, m, "grokbot"), session(t, m, "muse")
	req, err := m.Client(t, "grokbot").Send(t.Context(), "muse", "book dinner", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	call(t, handler, "check_inbox", nil)
	if out := call(t, handler, "reply", map[string]any{"request_id": req.ID, "message": "where?", "status": "needs_input"}); strings.HasPrefix(out, "ERROR") {
		t.Fatal(out)
	}
	if out := call(t, asker, "check_inbox", nil); !strings.Contains(out, "muse needs more information") || !strings.Contains(out, "Answer with answer") {
		t.Fatal(out)
	}
	if out := call(t, asker, "answer", map[string]any{"request_id": req.ID, "message": "Nopa"}); !strings.Contains(out, "resumed") {
		t.Fatal(out)
	}
	if out := call(t, handler, "check_inbox", nil); !strings.Contains(out, "Nopa") || !strings.Contains(out, "where?") || !strings.Contains(out, "book dinner") {
		t.Fatal(out)
	}
	if out := call(t, handler, "reply", map[string]any{"request_id": req.ID, "message": "booked"}); strings.HasPrefix(out, "ERROR") {
		t.Fatal(out)
	}
	if out := call(t, asker, "get_reply", map[string]any{"request_id": req.ID}); !strings.Contains(out, "booked") {
		t.Fatal(out)
	}
}
