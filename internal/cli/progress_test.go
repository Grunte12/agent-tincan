package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestProgressCLI(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	ctx := context.Background()
	grok, muse := m.Client(t, "grokbot"), m.Client(t, "muse")
	req, err := grok.Send(ctx, "muse", "work", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := muse.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("muse"), Agent: "muse"})
	if out, err := run(t, progressCmd(), req.ID, "calling now"); err != nil || !strings.Contains(out, "Progress recorded") {
		t.Fatalf("progress: %q %v", out, err)
	}
	res, err := grok.Get(ctx, req.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatTrace(traceResp{TraceID: req.ID, Steps: []envelope.Result{res}}); !strings.Contains(got, "claimed by muse") || !strings.Contains(got, "calling now") {
		t.Fatalf("trace: %s", got)
	}
}
