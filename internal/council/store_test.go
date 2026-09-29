package council

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func putCouncil(t *testing.T, s *Store, rec CouncilRecord) {
	t.Helper()
	if err := s.PutCouncil(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func TestStoreFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Council")
	path := filepath.Join(dir, "council.db")
	openStore(t, path)

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("folder mode = %o, want 700", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
}

func TestStoreReturnsCompletedReply(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "council.db"))
	putCouncil(t, s, CouncilRecord{RequestID: "r1", State: CouncilRunning, Request: `{"id":"r1"}`})
	putCouncil(t, s, CouncilRecord{
		RequestID:  "r1",
		State:      CouncilCompleted,
		Request:    `{"id":"r1"}`,
		Category:   "debugging",
		Reply:      "Recommendation: use a mutex.",
		ReportPath: "/tmp/r1/report.html",
		CardPath:   "/tmp/r1/card.png",
	})

	got, err := s.Council(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != CouncilCompleted || got.Reply != "Recommendation: use a mutex." ||
		got.ReportPath != "/tmp/r1/report.html" || got.CardPath != "/tmp/r1/card.png" ||
		got.Category != "debugging" || got.Request != `{"id":"r1"}` {
		t.Errorf("stored council = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.Before(got.CreatedAt) {
		t.Errorf("times: created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}

	if _, err := s.Council(ctx, "missing"); !errors.Is(err, ErrCouncilNotFound) {
		t.Errorf("missing council err = %v, want ErrCouncilNotFound", err)
	}
}

func TestStoreListsUnfinishedAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "council.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []CouncilRecord{
		{RequestID: "queued", State: CouncilQueued, Request: "q"},
		{RequestID: "running", State: CouncilRunning, Request: "r"},
		{RequestID: "done", State: CouncilCompleted},
		{RequestID: "failed", State: CouncilFailed},
		{RequestID: "declined", State: CouncilDeclined},
	} {
		putCouncil(t, s, rec)
		time.Sleep(2 * time.Millisecond) // distinct created times for ordering
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, path)
	got, err := s.UnfinishedCouncils(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RequestID != "queued" || got[1].RequestID != "running" {
		t.Fatalf("unfinished = %+v, want queued then running", got)
	}
	if got[0].Request != "q" || got[1].Request != "r" {
		t.Errorf("unfinished requests = %q, %q", got[0].Request, got[1].Request)
	}
}

func TestStoreRecordScoresIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "council.db"))
	putCouncil(t, s, CouncilRecord{RequestID: "r1", State: CouncilCompleted, Category: "writing"})
	scores := []ScoreRow{
		{Member: "claude-web", Score: 0.9, Placement: 1, Ballots: 4},
		{Member: "gemini-web", Score: 0.4, Placement: 2, Ballots: 4},
	}
	for i := range 2 {
		if err := s.RecordScores(ctx, "r1", "writing", scores); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scores WHERE request_id = 'r1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("score rows = %d, want 2", n)
	}
	board, err := s.Leaderboard(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range board {
		if row.Councils != 1 {
			t.Errorf("%s councils = %d, want 1", row.Member, row.Councils)
		}
	}
}

func TestStoreFailedCouncilRecordsNoScores(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "council.db"))
	putCouncil(t, s, CouncilRecord{RequestID: "r1", State: CouncilFailed})

	err := s.RecordScores(ctx, "r1", "debugging", []ScoreRow{{Member: "grok-web", Score: 1, Placement: 1, Ballots: 3}})
	if !errors.Is(err, ErrCouncilNotCompleted) {
		t.Errorf("err = %v, want ErrCouncilNotCompleted", err)
	}
	if err := s.RecordScores(ctx, "unknown", "debugging", []ScoreRow{{Member: "grok-web", Score: 1, Placement: 1, Ballots: 3}}); !errors.Is(err, ErrCouncilNotFound) {
		t.Errorf("unknown council err = %v, want ErrCouncilNotFound", err)
	}
	board, err := s.Leaderboard(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 0 {
		t.Errorf("leaderboard = %+v, want empty", board)
	}
}

func TestStoreLeaderboard(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, filepath.Join(t.TempDir(), "council.db"))
	record := func(id, category string, rows ...ScoreRow) {
		t.Helper()
		putCouncil(t, s, CouncilRecord{RequestID: id, State: CouncilCompleted, Category: category})
		if err := s.RecordScores(ctx, id, category, rows); err != nil {
			t.Fatal(err)
		}
	}
	record("c1", "debugging",
		ScoreRow{Member: "claude-web", Score: 0.8, Placement: 1, Ballots: 3},
		ScoreRow{Member: "chatgpt-web", Score: 0.6, Placement: 2, Ballots: 3},
		ScoreRow{Member: "gemini-web", Score: 0.1, Placement: 3, Ballots: 3},
	)
	record("c2", "debugging",
		ScoreRow{Member: "chatgpt-web", Score: 1.0, Placement: 1, Ballots: 3},
		ScoreRow{Member: "claude-web", Score: 0.5, Placement: 2, Ballots: 3},
		ScoreRow{Member: "gemini-web", Score: 0.0, Placement: 3, Ballots: 3},
	)
	record("c3", "writing",
		ScoreRow{Member: "gemini-web", Score: 0.9, Placement: 1, Ballots: 3},
		ScoreRow{Member: "claude-web", Score: 0.2, Placement: 2, Ballots: 3},
		ScoreRow{Member: "grok-web", Score: 0.1, Placement: 3, Ballots: 3},
	)

	type want struct {
		member   string
		councils int
		mean     float64
		wins     int
	}
	check := func(category string, wants []want) {
		t.Helper()
		got, err := s.Leaderboard(ctx, category)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(wants) {
			t.Fatalf("%q leaderboard = %+v, want %d rows", category, got, len(wants))
		}
		for i, w := range wants {
			g := got[i]
			if g.Member != w.member || g.Councils != w.councils || g.Wins != w.wins || math.Abs(g.MeanScore-w.mean) > 1e-9 {
				t.Errorf("%q row %d = %+v, want %+v", category, i, g, w)
			}
		}
	}

	// Overall: ordered by wins, then mean score.
	check("", []want{
		{"chatgpt-web", 2, 0.8, 1},
		{"claude-web", 3, 0.5, 1},
		{"gemini-web", 3, 1.0 / 3, 1},
		{"grok-web", 1, 0.1, 0},
	})
	check("debugging", []want{
		{"chatgpt-web", 2, 0.8, 1},
		{"claude-web", 2, 0.65, 1},
		{"gemini-web", 2, 0.05, 0},
	})
	check("writing", []want{
		{"gemini-web", 1, 0.9, 1},
		{"claude-web", 1, 0.2, 0},
		{"grok-web", 1, 0.1, 0},
	})
}
