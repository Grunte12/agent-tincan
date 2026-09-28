package client_test

import (
	"context"
	"fmt"
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
	if _, err := target.Peek(ctx, 0); err != nil {
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
	rest, _, err := client.AnswerPings(ctx, target, in, "inbox")
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
	raw, _ := http.NewRequestWithContext(ctx, "GET", m.URL("muse")+"/v1/poll?peek=1&hold=0", nil)
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

type failingPingResponder struct {
	phase  string
	status int
}

func (f failingPingResponder) Claim(context.Context, string) (envelope.Request, error) {
	if f.phase == "claim" {
		return envelope.Request{}, &client.APIError{Code: f.status}
	}
	return envelope.Request{}, nil
}
func (f failingPingResponder) Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error) {
	return envelope.Reply{}, &client.APIError{Code: f.status}
}
func TestAnswerPingsFailurePreservesInbox(t *testing.T) {
	oldRetry := client.PongRetry
	client.PongRetry = nil
	t.Cleanup(func() { client.PongRetry = oldRetry })
	for _, phase := range []string{"claim", "reply"} {
		for _, status := range []int{409, 503} {
			t.Run(fmt.Sprintf("%s/%d", phase, status), func(t *testing.T) {
				in := client.Inbox{Requests: []envelope.Request{{ID: "ping", Kind: envelope.KindPing}, {ID: "work", Kind: envelope.KindAsk}}, Replies: []client.Result{{Request: envelope.Request{ID: "reply", Kind: envelope.KindAsk}}}}
				rest, _, err := client.AnswerPings(t.Context(), failingPingResponder{phase, status}, in, "test")
				if (err == nil) != (status == 409) {
					t.Fatalf("error: %v", err)
				}
				if len(rest.Requests) != 1 || rest.Requests[0].ID != "work" || len(rest.Replies) != 1 || rest.Replies[0].Request.ID != "reply" {
					t.Fatalf("lost work: %+v", rest)
				}
			})
		}
	}
}
