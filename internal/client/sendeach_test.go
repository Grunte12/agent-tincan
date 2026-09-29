package client_test

import (
	"errors"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// Each target gets its own body and files, all under one group id and the
// explicit parent.
func TestSendEachPerTargetBodiesUnderOneGroup(t *testing.T) {
	m := attachMesh(t)
	ctx := t.Context()
	council := m.JoinOnMachineOf(t, "instinct", "council")
	parent, err := m.Client(t, "grokbot").Send(ctx, "council", "question", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := council.Claim(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	g, errs, err := council.SendEach(ctx, []client.Outgoing{
		{To: "muse", Body: "new chat\nfor muse"},
		{To: "instinct", Body: "new chat\nfor instinct", Files: []client.OutgoingFile{{Name: "plan.md", MIME: "text/markdown", Data: []byte("# plan")}}},
	}, envelope.KindAsk, parent.ID)
	if err != nil || len(errs) != 2 || errs[0] != nil || errs[1] != nil {
		t.Fatalf("SendEach = %v, %v", errs, err)
	}
	if len(g.Results) != 2 || g.Group == "" {
		t.Fatalf("group = %+v", g)
	}
	muse, inst := g.Results[0].Request, g.Results[1].Request
	if muse.To != "muse" || inst.To != "instinct" || muse.Group != g.Group || inst.Group != g.Group {
		t.Fatalf("requests = %+v / %+v", muse, inst)
	}
	if muse.ParentID != parent.ID || inst.ParentID != parent.ID || muse.TraceID != parent.TraceID {
		t.Fatalf("parents = %q %q, want %q", muse.ParentID, inst.ParentID, parent.ID)
	}
	if muse.Body != "new chat\nfor muse" || inst.Body != "new chat\nfor instinct" {
		t.Fatalf("bodies = %q / %q", muse.Body, inst.Body)
	}
	if len(muse.Attachments) != 0 || len(inst.Attachments) != 1 || inst.Attachments[0].Name != "plan.md" {
		t.Fatalf("attachments = %+v / %+v", muse.Attachments, inst.Attachments)
	}
	data, _, err := m.Client(t, "instinct").FetchAttachment(ctx, inst.Attachments[0].ID)
	if err != nil || string(data) != "# plan" {
		t.Fatalf("fetch = %q, %v", data, err)
	}
}

// A target whose files cannot be uploaded is not sent, and its error says
// so, while the rest of the batch goes out.
func TestSendEachUploadFailureSkipsThatTarget(t *testing.T) {
	m := testrelay.New(t, relay.Config{}) // stores no attachments
	ctx := t.Context()
	c := m.Client(t, "grokbot")
	g, errs, err := c.SendEach(ctx, []client.Outgoing{
		{To: "muse", Body: "plain"},
		{To: "instinct", Body: "with file", Files: []client.OutgoingFile{{Name: "a.txt", Data: []byte("x")}}},
	}, envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] != nil || g.Results[0].Request.ID == "" {
		t.Fatalf("muse: %v %+v", errs[0], g.Results[0])
	}
	if !errors.Is(errs[1], client.ErrUploadFailed) || g.Results[1].Request.ID != "" || g.Results[1].Status != envelope.StatusFailed {
		t.Fatalf("instinct: %v %+v", errs[1], g.Results[1])
	}
	in, err := m.Client(t, "instinct").Poll(ctx, 0)
	if err != nil || len(in.Requests) != 0 {
		t.Fatalf("instinct inbox = %+v, %v", in, err)
	}
}

func TestSendEachRefusesBadBatches(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	c := m.Client(t, "grokbot")
	nine := make([]client.Outgoing, 9)
	for i := range nine {
		nine[i] = client.Outgoing{To: "muse", Body: "x"}
	}
	for name, outs := range map[string][]client.Outgoing{
		"none":  nil,
		"nine":  nine,
		"twice": {{To: "muse", Body: "a"}, {To: "muse", Body: "b"}},
		"self":  {{To: "grokbot", Body: "a"}},
	} {
		if _, _, err := c.SendEach(t.Context(), outs, envelope.KindAsk, ""); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
}
