package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestNotificationsAreBodyFreeScopedAndReadOnly(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	const museSession = "00000000-0000-4000-8000-000000000001"
	const otherSession = "00000000-0000-4000-8000-000000000002"
	const grokSession = "00000000-0000-4000-8000-000000000003"

	queued := ask(t, s, "grokbot", "muse", "private request body")
	if _, err := s.DB().Exec(`UPDATE requests SET to_session = ? WHERE id = ?`, museSession, queued.ID); err != nil {
		t.Fatal(err)
	}
	_ = ask(t, s, "grokbot", "muse", "unscoped request")
	wrongAgent := ask(t, s, "grokbot", "instinct", "other recipient")
	if _, err := s.DB().Exec(`UPDATE requests SET to_session = ? WHERE id = ?`, otherSession, wrongAgent.ID); err != nil {
		t.Fatal(err)
	}
	replied := ask(t, s, "grokbot", "muse", "private reply source")
	if _, err := s.DB().Exec(`UPDATE requests SET from_session = ? WHERE id = ?`, grokSession, replied.ID); err != nil {
		t.Fatal(err)
	}
	reply(t, s, replied, envelope.StatusAnswered, "private reply body")
	otherReply := ask(t, s, "instinct", "muse", "another agent request")
	if _, err := s.DB().Exec(`UPDATE requests SET from_session = ? WHERE id = ?`, otherSession, otherReply.ID); err != nil {
		t.Fatal(err)
	}
	reply(t, s, otherReply, envelope.StatusAnswered, "another agent reply")

	_, beforeStatus, err := s.Request(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSeen, err := s.CountUnseenReplies(ctx, "grokbot")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Notifications(ctx, "grokbot")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != replied.ID || got[0].SessionID != grokSession || got[0].Kind != "reply" || got[0].Generation == 0 {
		t.Fatalf("grok notifications = %+v", got)
	}
	got, err = s.Notifications(ctx, "muse")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != queued.ID || got[0].SessionID != museSession || got[0].Kind != "request" {
		t.Fatalf("muse notifications = %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "body") {
		t.Fatalf("notification leaked content: %s", raw)
	}
	_, afterStatus, err := s.Request(ctx, queued.ID)
	if err != nil || afterStatus != beforeStatus {
		t.Fatalf("request status changed from %s to %s: %v", beforeStatus, afterStatus, err)
	}
	afterSeen, err := s.CountUnseenReplies(ctx, "grokbot")
	if err != nil || afterSeen != beforeSeen {
		t.Fatalf("unseen replies changed from %d to %d: %v", beforeSeen, afterSeen, err)
	}
	// Verify the observer did not lease or otherwise claim the queued request.
	if beforeStatus != envelope.StatusQueued {
		t.Fatalf("request status before observation = %s", beforeStatus)
	}
}
