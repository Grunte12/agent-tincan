package council

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrCouncilNotFound means no council is stored for that request id.
	ErrCouncilNotFound = errors.New("council not found")
	// ErrCouncilNotCompleted means the council did not complete, so it has no
	// scores to record.
	ErrCouncilNotCompleted = errors.New("council not completed")
)

// CouncilState is where a council stands. Queued and running councils are
// unfinished and are re-enqueued on restart; the rest are final.
type CouncilState string

const (
	CouncilQueued    CouncilState = "queued"
	CouncilRunning   CouncilState = "running"
	CouncilCompleted CouncilState = "completed"
	CouncilFailed    CouncilState = "failed"
	CouncilDeclined  CouncilState = "declined"
)

// CouncilRecord is one council, keyed by its convening request id.
type CouncilRecord struct {
	RequestID  string
	State      CouncilState
	Request    string // the serialized convening request, so a restart can run it again
	Category   string
	Reply      string // the final reply text, resent when the request is redelivered
	ReportPath string
	CardPath   string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ScoreRow is one member's peer score in one completed council.
type ScoreRow struct {
	RequestID string
	Member    string
	Score     float64
	Placement int // 1 is the council's top answer
	Ballots   int // valid peer ballots behind the score
	Category  string
	CreatedAt time.Time
}

// LeaderboardRow is one member's standing across scored councils.
type LeaderboardRow struct {
	Member    string
	Councils  int
	MeanScore float64
	Wins      int
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS councils (
  request_id  TEXT PRIMARY KEY,
  state       TEXT NOT NULL,
  request     TEXT NOT NULL DEFAULT '',
  category    TEXT NOT NULL DEFAULT '',
  reply       TEXT NOT NULL DEFAULT '',
  report_path TEXT NOT NULL DEFAULT '',
  card_path   TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS councils_state ON councils(state);
CREATE TABLE IF NOT EXISTS scores (
  request_id TEXT NOT NULL REFERENCES councils(request_id),
  member     TEXT NOT NULL,
  score      REAL NOT NULL,
  placement  INTEGER NOT NULL,
  ballots    INTEGER NOT NULL,
  category   TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (request_id, member)
);
CREATE INDEX IF NOT EXISTS scores_category ON scores(category);
`

// Store is Council's own SQLite database of councils and scores.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// OpenStore opens (creating if needed) the database at path. The folder is
// created 0700 and the file 0600, since stored replies carry model answers.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("council store: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("council store: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("council store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("council store: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialize in-process
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// PutCouncil inserts or updates the council for rec.RequestID, keeping its
// original creation time.
func (s *Store) PutCouncil(ctx context.Context, rec CouncilRecord) error {
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO councils (request_id, state, request, category, reply, report_path, card_path, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(request_id) DO UPDATE SET
  state = excluded.state, request = excluded.request, category = excluded.category,
  reply = excluded.reply, report_path = excluded.report_path, card_path = excluded.card_path,
  updated_at = excluded.updated_at`,
		rec.RequestID, string(rec.State), rec.Request, rec.Category, rec.Reply, rec.ReportPath, rec.CardPath, now, now)
	return err
}

const councilColumns = `request_id, state, request, category, reply, report_path, card_path, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanCouncil(r rowScanner) (CouncilRecord, error) {
	var rec CouncilRecord
	var state string
	var created, updated int64
	if err := r.Scan(&rec.RequestID, &state, &rec.Request, &rec.Category, &rec.Reply, &rec.ReportPath, &rec.CardPath, &created, &updated); err != nil {
		return CouncilRecord{}, err
	}
	rec.State = CouncilState(state)
	rec.CreatedAt = time.UnixMilli(created)
	rec.UpdatedAt = time.UnixMilli(updated)
	return rec, nil
}

// Council returns the stored council for a convening request id.
func (s *Store) Council(ctx context.Context, requestID string) (CouncilRecord, error) {
	rec, err := scanCouncil(s.db.QueryRowContext(ctx, `SELECT `+councilColumns+` FROM councils WHERE request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return CouncilRecord{}, ErrCouncilNotFound
	}
	return rec, err
}

// UnfinishedCouncils returns queued and running councils, oldest first.
func (s *Store) UnfinishedCouncils(ctx context.Context) ([]CouncilRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+councilColumns+` FROM councils WHERE state IN (?, ?) ORDER BY created_at, rowid`,
		string(CouncilQueued), string(CouncilRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CouncilRecord
	for rows.Next() {
		rec, err := scanCouncil(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RecordScores stores the peer scores of a completed council under category.
// It is idempotent per request id: once a council has scores, later calls
// change nothing, so a restart never double-counts.
func (s *Store) RecordScores(ctx context.Context, requestID, category string, scores []ScoreRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM councils WHERE request_id = ?`, requestID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCouncilNotFound
	}
	if err != nil {
		return err
	}
	if CouncilState(state) != CouncilCompleted {
		return ErrCouncilNotCompleted
	}
	var scored bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM scores WHERE request_id = ?)`, requestID).Scan(&scored); err != nil {
		return err
	}
	if scored {
		return nil
	}
	now := s.now().UnixMilli()
	for _, sc := range scores {
		if _, err := tx.ExecContext(ctx, `INSERT INTO scores (request_id, member, score, placement, ballots, category, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			requestID, sc.Member, sc.Score, sc.Placement, sc.Ballots, category, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Leaderboard ranks members by wins, then mean peer score. An empty category
// covers every council; otherwise only councils in that category count.
func (s *Store) Leaderboard(ctx context.Context, category string) ([]LeaderboardRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT member, COUNT(*), AVG(score), SUM(placement = 1)
FROM scores
WHERE ? = '' OR category = ?
GROUP BY member
ORDER BY SUM(placement = 1) DESC, AVG(score) DESC, member`, category, category)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaderboardRow
	for rows.Next() {
		var r LeaderboardRow
		if err := rows.Scan(&r.Member, &r.Councils, &r.MeanScore, &r.Wins); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
