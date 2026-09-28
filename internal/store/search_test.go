package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestSearchBodiesRepliesAndLiteralTerms(t *testing.T) {
	s, c := open(t, ":memory:")
	req := ask(t, s, "a", "b", "restaurant OR sushi")
	c.advance(time.Second)
	newest := ask(t, s, "a", "b", "restaurant dinner")
	hits, err := s.Search(t.Context(), "restaurant", "a", 1)
	if err != nil || len(hits) != 1 || hits[0].RequestID != newest.ID || hits[0].Status != envelope.StatusQueued || hits[0].CreatedAt != newest.CreatedAt || !strings.Contains(hits[0].Snippet, "[restaurant]") {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	for _, query := range []string{`restaurant OR sushi`, `"restaurant" sushi`, `restaurant "OR" sushi`} {
		hits, err := s.Search(t.Context(), query, "b", 20)
		if err != nil || len(hits) != 1 || hits[0].RequestID != req.ID {
			t.Fatalf("query %q: %+v, %v", query, hits, err)
		}
	}
	for _, query := range []string{`"`, `()`, `*`, `NEAR(restaurant)`, `body:restaurant`, `restaurant NOT sushi`, "", "  "} {
		hits, err := s.Search(t.Context(), query, "", 20)
		if err != nil || len(hits) != 0 {
			t.Fatalf("query %q: %+v, %v", query, hits, err)
		}
	}
	if hits, err := s.Search(t.Context(), "Tuesday", "a", 20); err != nil || len(hits) != 0 {
		t.Fatalf("before reply = %+v, %v", hits, err)
	}
	if _, err := s.Reply(t.Context(), req.ID, "b", envelope.Reply{Body: "confirmed Tuesday", Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(t.Context(), "Tuesday", "a", 20)
	if err != nil || len(hits) != 1 || hits[0].Status != envelope.StatusAnswered || !strings.Contains(hits[0].ReplySnippet, "[Tuesday]") {
		t.Fatalf("reply = %+v, %v", hits, err)
	}
	if hits, err := s.Search(t.Context(), "Tuesday", "outsider", 20); err != nil || len(hits) != 0 {
		t.Fatalf("outsider = %+v, %v", hits, err)
	}
	for range 55 {
		ask(t, s, "a", "b", "restaurant")
	}
	for _, tc := range []struct{ limit, want int }{{0, 20}, {100, 50}} {
		hits, err := s.Search(t.Context(), "restaurant", "", tc.limit)
		if err != nil || len(hits) != tc.want {
			t.Fatalf("limit %d: %d, %v", tc.limit, len(hits), err)
		}
	}
}

func TestSearchBackfillAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO requests(id,from_agent,to_agent,trace_id,hop,chain,kind,body,status,created_at,updated_at,expires_at,attachments)
 VALUES ('old','a','b','old',1,'["a"]','ask','restaurant','answered',1000,1000,2000,'[{"id":"file","name":"menu.pdf"}]');
 INSERT INTO replies(request_id,from_agent,status,body,created_at,attachments) VALUES ('old','b','answered','Tuesday',1000,'[{"id":"reply-file","name":"receipt.pdf"}]');`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"restaurant", "Tuesday"} {
			hits, err := s.Search(t.Context(), q, "a", 20)
			if err != nil || len(hits) != 1 || hits[0].RequestID != "old" || strings.Join(hits[0].AttachmentNames, ",") != "menu.pdf,receipt.pdf" {
				t.Fatalf("backfill %q: %+v, %v", q, hits, err)
			}
		}
		if hits, err := s.Search(t.Context(), "menu", "", 20); err != nil || len(hits) != 0 {
			t.Fatalf("attachment name indexed: %+v, %v", hits, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := open(t, path)
	ask(t, s, "a", "b", "new message")
	if hits, err := s.Search(t.Context(), "new", "b", 20); err != nil || len(hits) != 1 {
		t.Fatalf("after reopen = %+v, %v", hits, err)
	}
}

func TestSearchCandidateCap(t *testing.T) {
	s, _ := open(t, ":memory:")
	old := ask(t, s, "visible", "b", "common")
	// Give the old request the newest timestamp: the candidate cap precedes
	// timestamp ordering but counts only matches the caller may see.
	if _, err := s.db.Exec("UPDATE requests SET created_at = 9999999999999 WHERE id = ?", old.ID); err != nil {
		t.Fatal(err)
	}
	for range searchCandidateLimit {
		ask(t, s, "other", "b", "common")
	}
	// 2000 newer matches in chains "visible" cannot see do not crowd out its own.
	hits, err := s.Search(t.Context(), "common", "visible", 50)
	if err != nil || len(hits) != 1 || hits[0].RequestID != old.ID {
		t.Fatalf("visible match lost behind the cap: %+v, %v", hits, err)
	}
	hits, err = s.Search(t.Context(), "common", "", 1)
	if err != nil || len(hits) != 1 || hits[0].RequestID == old.ID {
		t.Fatalf("ordering before cap: %+v, %v", hits, err)
	}
}

func TestSearchSplitExcerpts(t *testing.T) {
	s, _ := open(t, ":memory:")
	req := ask(t, s, "a", "b", "restaurant booking")
	if _, err := s.Reply(t.Context(), req.ID, "b", envelope.Reply{Body: "confirmed Tuesday", Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(t.Context(), "restaurant Tuesday", "", 20)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Snippet, "[restaurant]") || !strings.Contains(hits[0].ReplySnippet, "[Tuesday]") {
		t.Fatalf("split: %+v, %v", hits, err)
	}
	for _, tc := range []struct {
		query   string
		request bool
	}{{"restaurant", true}, {"Tuesday", false}} {
		hits, err := s.Search(t.Context(), tc.query, "", 20)
		if err != nil || len(hits) != 1 {
			t.Fatalf("%s: %+v, %v", tc.query, hits, err)
		}
		if (hits[0].Snippet != "") != tc.request || (hits[0].ReplySnippet != "") == tc.request {
			t.Fatalf("unmatched excerpt: %+v", hits[0])
		}
	}
}

func TestSearchBackfillBatchFailureAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for range searchBackfillBatchSize + 3 {
		ask(t, s, "a", "b", "historical")
	}
	// Simulate an unindexed history and a write failure in the second batch.
	_, err = s.db.Exec(`DELETE FROM requests_fts;
 CREATE TRIGGER fail_backfill BEFORE UPDATE ON search_backfill WHEN new.high_water > 500
 BEGIN SELECT RAISE(ABORT, 'injected backfill failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("backfill failure prevented Open: %v", err)
	}
	var high, count int
	if err := s.db.QueryRow("SELECT high_water FROM search_backfill").Scan(&high); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM requests_fts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if high != searchBackfillBatchSize || count != searchBackfillBatchSize {
		t.Fatalf("checkpoint=%d indexed=%d", high, count)
	}
	req := ask(t, s, "a", "b", "fresh")
	if _, err := s.Reply(t.Context(), req.ID, "b", envelope.Reply{Body: "live", Status: envelope.StatusAnswered}); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.Search(t.Context(), "fresh live", "", 20); err != nil || len(hits) != 1 {
		t.Fatalf("live triggers: %+v, %v", hits, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_backfill"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.QueryRow("SELECT count(*) FROM requests_fts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != searchBackfillBatchSize+4 {
		t.Fatalf("resumed count=%d", count)
	}
	if err := s.db.QueryRow("SELECT high_water FROM search_backfill").Scan(&high); err != nil {
		t.Fatal(err)
	}
	if high != count {
		t.Fatalf("resumed checkpoint=%d count=%d", high, count)
	}
}

// An agent's older chains are still searched when the newest chunk of its
// visible requests has no match, and chains it cannot see never are.
func TestSearchWalksOlderVisibleChunks(t *testing.T) {
	old := searchVisibleWindow
	searchVisibleWindow = 3
	t.Cleanup(func() { searchVisibleWindow = old })
	s, _ := open(t, ":memory:")
	rare := ask(t, s, "visible", "b", "the rare zanzibar note")
	for range 7 {
		ask(t, s, "visible", "b", "common chatter")
		ask(t, s, "other", "c", "zanzibar elsewhere")
	}
	hits, err := s.Search(t.Context(), "zanzibar", "visible", 20)
	if err != nil || len(hits) != 1 || hits[0].RequestID != rare.ID {
		t.Fatalf("older visible match = %+v, %v", hits, err)
	}
	hits, err = s.Search(t.Context(), "common", "visible", 5)
	if err != nil || len(hits) != 5 {
		t.Fatalf("limit across chunks = %d hits, %v", len(hits), err)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].CreatedAt.After(hits[i-1].CreatedAt) {
			t.Fatalf("not newest first: %+v", hits)
		}
	}
}

// A request held for the owner and never approved is readable only by its
// sender and admins, so search hides it from everyone else in the chain,
// the target included, until the owner approves it.
func TestSearchHidesHeldUntilApproved(t *testing.T) {
	s, _ := open(t, ":memory:")
	req, err := s.Enqueue(t.Context(), envelope.Request{From: "a", To: "b", Body: "secret dinner plan", Status: envelope.StatusHeld, HoldTTL: time.Hour}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"a", ""} {
		if hits, err := s.Search(t.Context(), "secret", who, 20); err != nil || len(hits) != 1 || hits[0].RequestID != req.ID {
			t.Fatalf("%q: hits = %+v, %v", who, hits, err)
		}
	}
	if hits, err := s.Search(t.Context(), "secret", "b", 20); err != nil || len(hits) != 0 {
		t.Fatalf("target sees held request: %+v, %v", hits, err)
	}
	if _, err := s.Release(t.Context(), req.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if hits, err := s.Search(t.Context(), "secret", "b", 20); err != nil || len(hits) != 1 {
		t.Fatalf("approved request hidden: %+v, %v", hits, err)
	}
}

func TestSearchLabelsClarificationQuestions(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := t.Context()
	req := ask(t, s, "a", "b", "book dinner")
	question := envelope.Reply{Status: envelope.StatusNeedsInput, Body: "which restaurant?"}
	claimAndAsk := func() {
		t.Helper()
		if _, err := s.Claim(ctx, req.ID, "b", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, "b", question); err != nil {
			t.Fatal(err)
		}
	}
	wantQuestion := func(step string, status envelope.Status) {
		t.Helper()
		hits, err := s.Search(ctx, "restaurant", "a", 20)
		if err != nil || len(hits) != 1 || hits[0].Status != status || hits[0].ReplySnippet != "" || !strings.Contains(hits[0].QuestionSnippet, "[restaurant]") {
			t.Fatalf("%s: %+v, %v", step, hits, err)
		}
	}
	claimAndAsk()
	wantQuestion("waiting", envelope.StatusNeedsInput)
	if _, err := s.Answer(ctx, req.ID, "a", "Nopa"); err != nil {
		t.Fatal(err)
	}
	wantQuestion("answered question", envelope.StatusQueued)
	if _, err := s.Reply(ctx, req.ID, "b", envelope.Reply{Status: envelope.StatusAnswered, Body: "booked Nopa"}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search(ctx, "Nopa", "a", 20)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].ReplySnippet, "[Nopa]") || hits[0].QuestionSnippet != "" {
		t.Fatalf("final reply: %+v, %v", hits, err)
	}

	// A question left waiting when the request expires is history, not a reply.
	req = ask(t, s, "a", "b", "book lunch")
	claimAndAsk()
	c.advance(2 * time.Hour)
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	hits, err = s.Search(ctx, "restaurant", "a", 20)
	if err != nil || len(hits) != 1 || hits[0].RequestID != req.ID || hits[0].Status != envelope.StatusExpired || hits[0].ReplySnippet != "" || !strings.Contains(hits[0].QuestionSnippet, "[restaurant]") {
		t.Fatalf("expired: %+v, %v", hits, err)
	}
}
