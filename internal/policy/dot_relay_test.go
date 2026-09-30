package policy_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

type wakes struct {
	mu sync.Mutex
	to []string
}

func (w *wakes) Queued(_ context.Context, r envelope.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.to = append(w.to, r.To)
}

func (w *wakes) count(agent string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, to := range w.to {
		if to == agent {
			n++
		}
	}
	return n
}

// End to end through a real relay with no approval.json: an admin invites
// a dot with --kind dot, a teammate's ask to it is held and wakes nothing,
// the owner's approval queues it and wakes the dot, and the reply to the
// dot's own ask comes back without a hold.
func TestDotHoldAndApproveThroughRelay(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	w := &wakes{}
	m.Server.SetEvents(w)
	admin := m.Client(t, "admin")
	if code, err := admin.InviteKind(t.Context(), "dot", onboard.KindDot); err != nil || code == "" {
		t.Fatalf("invite --kind dot: %q %v", code, err)
	}
	dot := m.JoinOnMachineOf(t, "instinct", "dot")
	if err := admin.SetKind(t.Context(), "dot", onboard.KindDot); err != nil {
		t.Fatal(err)
	}
	muse := m.Client(t, "muse")

	req, err := muse.Send(t.Context(), "dot", "summarize my inbox", envelope.KindAsk, "", false)
	if err != nil || req.Status != envelope.StatusHeld {
		t.Fatalf("ask to dot = %q %v", req.Status, err)
	}
	if w.count("dot") != 0 || m.Server.QueuedCount("dot") != 0 {
		t.Fatal("held request queued or woke the dot")
	}
	if err := admin.Raw(t.Context(), "POST", "/v1/admin/requests/"+req.ID+"/approve", nil, nil); err != nil {
		t.Fatal(err)
	}
	if w.count("dot") != 1 || m.Server.QueuedCount("dot") != 1 {
		t.Fatalf("approval did not queue and wake the dot: wakes %d queued %d", w.count("dot"), m.Server.QueuedCount("dot"))
	}
	inbox, err := dot.Poll(t.Context(), time.Millisecond)
	if err != nil || len(inbox.Requests) != 1 || inbox.Requests[0].ID != req.ID {
		t.Fatalf("dot inbox after approval: %+v %v", inbox, err)
	}

	// The dot's own ask is not held, and neither is the reply to it.
	own, err := dot.Send(t.Context(), "grokbot", "what is on the calendar", envelope.KindAsk, "", false)
	if err != nil || own.Status == envelope.StatusHeld {
		t.Fatalf("dot's own ask = %q %v", own.Status, err)
	}
	grok := m.Client(t, "grokbot")
	if _, err := grok.Claim(t.Context(), own.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := grok.Reply(t.Context(), own.ID, "two meetings", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	res, err := dot.Get(t.Context(), own.ID, time.Second)
	if err != nil || res.Status != envelope.StatusAnswered || res.Reply == nil || res.Reply.Body != "two meetings" {
		t.Fatalf("reply to the dot's own ask: %+v %v", res, err)
	}
}
