package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestHeldPersistenceAndReleaseTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	st.SetClock(func() time.Time { return now })
	req, err := st.Enqueue(t.Context(), envelope.Request{From: "sender", To: "target", Body: "hello", Chain: []string{"sender"}, Status: envelope.StatusHeld, HoldTTL: time.Hour}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetClock(func() time.Time { return now })
	now = now.Add(59 * time.Minute)
	list, err := st.Held(t.Context())
	if err != nil || len(list) != 1 || !list[0].WasHeld || list[0].Approved {
		t.Fatalf("persisted held: %+v %v", list, err)
	}
	if _, err := st.Release(t.Context(), req.ID, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := st.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	res, err := st.Get(t.Context(), req.ID, "sender")
	if err != nil || res.Status != envelope.StatusQueued || !res.Request.CreatedAt.Equal(req.CreatedAt) || !res.Request.WasHeld || !res.Request.Approved {
		t.Fatalf("release TTL: %+v %v", res, err)
	}
}

func TestExpiredHeldCannotBeDecidedBeforeSweep(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	st.SetClock(func() time.Time { return now })
	req, err := st.Enqueue(t.Context(), envelope.Request{From: "sender", To: "target", Chain: []string{"sender"}, Status: envelope.StatusHeld, HoldTTL: time.Hour}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err := st.Release(t.Context(), req.ID, time.Hour); err != ErrWrongState {
		t.Fatalf("release expired: %v", err)
	}
	if _, err := st.DenyHeld(t.Context(), req.ID, "no"); err != ErrWrongState {
		t.Fatalf("deny expired: %v", err)
	}
	transitions, err := st.Sweep(t.Context())
	if err != nil || len(transitions) != 1 || !transitions[0].Held || transitions[0].Status != envelope.StatusExpired {
		t.Fatalf("sweep: %+v %v", transitions, err)
	}
}

func TestHeldDecisionRace(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	req, err := st.Enqueue(t.Context(), envelope.Request{From: "sender", To: "target", Chain: []string{"sender"}, Status: envelope.StatusHeld, HoldTTL: time.Hour}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, err := st.Release(t.Context(), req.ID, time.Hour); results <- err }()
	go func() { _, err := st.DenyHeld(t.Context(), req.ID, "no"); results <- err }()
	first, second := <-results, <-results
	switch {
	case first == nil && second == ErrWrongState, second == nil && first == ErrWrongState:
	default:
		t.Fatalf("decision race: %v %v", first, second)
	}
	res, err := st.Get(t.Context(), req.ID, "sender")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == envelope.StatusDeclined && (res.Reply == nil || res.Reply.Body != "no") {
		t.Fatalf("denial reply lost: %+v", res)
	}
	if res.Status == envelope.StatusQueued && res.Reply != nil {
		t.Fatalf("approved with denial reply: %+v", res)
	}
}

func TestApprovalHistoryMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	states := []struct {
		status         envelope.Status
		event          string
		held, approved bool
	}{
		{envelope.StatusHeld, "", true, false},
		{envelope.StatusCancelled, "held", true, false},
		{envelope.StatusDeclined, "denied", true, false},
		{envelope.StatusExpired, "hold_expired", true, false},
		{envelope.StatusQueued, "approved", true, true},
		{envelope.StatusCancelled, "", false, false},
	}
	var ids []string
	for _, tc := range states {
		req, err := st.Enqueue(t.Context(), envelope.Request{From: "sender", To: "target", Body: "secret", Chain: []string{"sender"}}, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, req.ID)
		if _, err := st.db.Exec("UPDATE requests SET status = ? WHERE id = ?", tc.status, req.ID); err != nil {
			t.Fatal(err)
		}
		if tc.event != "" {
			if err := st.Audit(t.Context(), AuditEvent{Event: tc.event, RequestID: req.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, column := range []string{"was_held", "approved"} {
		if _, err := st.db.Exec("ALTER TABLE requests DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		st, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, tc := range states {
			req, _, err := st.Request(t.Context(), ids[i])
			if err != nil || req.WasHeld != tc.held || req.Approved != tc.approved {
				t.Fatalf("migration %d: %+v %v", i, req, err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
