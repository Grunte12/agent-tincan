package client_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestPingCapabilityAndAutomaticReply(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	sender, target := m.Client(t, "grokbot"), m.Client(t, "muse")
	ctx := t.Context()
	if _, err := sender.Send(ctx, "muse", "", envelope.KindPing, ""); !client.IsStatus(err, 409) {
		t.Fatalf("unadvertised ping: %v", err)
	}
	if _, err := target.Agents(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := sender.Send(ctx, "muse", "", envelope.KindPing, "")
	if err != nil {
		t.Fatal(err)
	}
	in, err := target.Poll(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := client.AnswerPings(ctx, target, in, "inbox")
	if err != nil || !rest.Empty() {
		t.Fatalf("rest: %+v, %v", rest, err)
	}
	got, err := sender.Get(ctx, req.ID, 0)
	if err != nil || got.Reply == nil || !strings.Contains(got.Reply.Body, "answered by inbox") {
		t.Fatalf("pong: %+v, %v", got, err)
	}
	in, err = sender.Poll(ctx, 0)
	if err != nil || !in.Empty() {
		t.Fatalf("pong leaked into sender inbox: %+v, %v", in, err)
	}
	// An older process replaces the capability advertisement on its next call.
	raw, _ := http.NewRequestWithContext(ctx, "GET", m.URL("muse")+"/v1/agents", nil)
	raw.Header.Set(client.VersionHeader, "0.5.4")
	resp, err := http.DefaultClient.Do(raw)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := sender.Send(ctx, "muse", "", envelope.KindPing, ""); !client.IsStatus(err, 409) {
		t.Fatalf("old client ping: %v", err)
	}
	features, err := m.Store.AgentFeatures(ctx)
	if err != nil || features["muse"] != "" {
		t.Fatalf("features: %v, %v", features, err)
	}
}
