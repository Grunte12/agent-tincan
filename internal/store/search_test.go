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
	if err != nil || len(hits) != 1 || hits[0].Status != envelope.StatusAnswered || !strings.Contains(hits[0].Snippet, "[Tuesday]") {
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
