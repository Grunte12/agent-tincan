package store

import (
	"context"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func ownerNoticeCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM owner_notices`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The owner's notice and its record are written together: a notice that
// cannot be queued leaves no record, so the next follow-up sends it, and
// one that was sent is not sent again.
func TestEnqueueOwnerNoticeIsAtomic(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	note := envelope.Request{From: "relay", To: "muse", Kind: envelope.KindNotify, Hop: 1, Chain: []string{"relay"}, Body: "grokbot has not checked in"}

	bad := note
	bad.Attachments = []envelope.Attachment{{ID: "no-such-attachment"}}
	if _, _, err := s.EnqueueOwnerNotice(ctx, "grokbot", 42, bad, time.Hour); err == nil {
		t.Fatal("queuing with a missing attachment worked")
	}
	if n := ownerNoticeCount(t, s); n != 0 {
		t.Fatalf("owner notice records after a failed enqueue = %d, want 0", n)
	}

	req, added, err := s.EnqueueOwnerNotice(ctx, "grokbot", 42, note, time.Hour)
	if err != nil || !added || req.ID == "" {
		t.Fatalf("EnqueueOwnerNotice = %+v, %v, %v", req, added, err)
	}
	if _, added, err := s.EnqueueOwnerNotice(ctx, "grokbot", 42, note, time.Hour); err != nil || added {
		t.Fatalf("second notice for the episode: added %v, err %v", added, err)
	}
	var queued int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM requests WHERE to_agent = 'muse'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued notices = %d, %v; want 1", queued, err)
	}
	// Another episode is told again.
	if _, added, err := s.EnqueueOwnerNotice(ctx, "grokbot", 43, note, time.Hour); err != nil || !added {
		t.Fatalf("next episode: added %v, err %v", added, err)
	}
}
