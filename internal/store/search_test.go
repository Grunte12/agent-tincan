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
	// Give the old request the newest timestamp: the candidate cap must precede
	// both timestamp ordering and visibility filtering.
	if _, err := s.db.Exec("UPDATE requests SET created_at = 9999999999999 WHERE id = ?", old.ID); err != nil {
		t.Fatal(err)
	}
	for range searchCandidateLimit {
		ask(t, s, "other", "b", "common")
	}
	hits, err := s.Search(t.Context(), "common", "visible", 50)
	if err != nil || len(hits) != 0 {
		t.Fatalf("outside cap: %+v, %v", hits, err)
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
