package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// AE1 in tincan agents: a schedule agent shows its interval, and an overdue
// one also shows how long ago it last checked its inbox.
func TestFormatAgentsSchedule(t *testing.T) {
	now := time.Now()
	got := formatAgents([]client.AgentInfo{
		{Name: "fo", Wake: "schedule", CheckEverySeconds: 300, ExpectReplySeconds: 600, LastPoll: now.Add(-3 * time.Minute)},
		{Name: "late", Wake: "schedule", CheckEverySeconds: 300, ExpectReplySeconds: 600, Overdue: true, LastPoll: now.Add(-25 * time.Minute)},
		{Name: "grokbot", Wake: "webhook", LastPoll: now},
	}, now)
	want := "fo             offline  wake=schedule (every 5m) last seen 3m ago\n" +
		"late           offline  wake=schedule (every 5m) last seen 25m ago overdue: last check 25m ago\n" +
		"grokbot        offline  wake=webhook last seen just now\n"
	if got != want {
		t.Fatalf("agents =\n%s\nwant\n%s", got, want)
	}
}

// AE2 through the CLI: ask with its default wait against a schedule target
// says how often it checks and when to expect a reply; against a webhook
// target the text is unchanged.
func TestAskScheduleTargetDefaultWait(t *testing.T) {
	m := testrelay.New(t, relay.Config{MaxWait: 200 * time.Millisecond})
	m.Server.SetWakeNamer(wake.New(wake.Config{
		"muse":    {Method: wake.Schedule, Every: "5m"},
		"grokbot": {Method: wake.Webhook, URL: "http://127.0.0.1:1/hook"},
	}, nil, wake.Options{}))
	useConfig(t, client.Config{Relay: m.URL("instinct"), Agent: "instinct"})

	out, _, err := runSplit(t, askCmd(), "muse", "reschedule the dentist")
	if err != nil || !strings.HasPrefix(out, "muse checks its inbox every 5m; expect a reply within about 10m.\nNo reply yet from muse.") {
		t.Fatalf("ask muse = %q, %v", out, err)
	}
	out, _, err = runSplit(t, askCmd(), "grokbot", "status")
	if err != nil || !strings.HasPrefix(out, "No reply yet from grokbot.") {
		t.Fatalf("ask grokbot = %q, %v", out, err)
	}
}
