package store

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const searchCandidateLimit = 2000

// searchVisibleWindow bounds an agent's search to its newest visible requests.
const searchVisibleWindow = 5000
const searchBackfillBatchSize = 500

func (s *Store) migrateSearch() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'requests_fts'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS search_backfill (id INTEGER PRIMARY KEY CHECK (id = 1), high_water INTEGER NOT NULL);
 INSERT OR IGNORE INTO search_backfill VALUES (1, 0);
 CREATE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);`)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.Exec(`
 CREATE VIRTUAL TABLE requests_fts USING fts5(request_id UNINDEXED, trace_id UNINDEXED, body, reply_body);
 CREATE TABLE search_backfill (id INTEGER PRIMARY KEY CHECK (id = 1), high_water INTEGER NOT NULL);
 INSERT INTO search_backfill VALUES (1, 0);
 CREATE TRIGGER requests_search_insert AFTER INSERT ON requests BEGIN
   INSERT INTO requests_fts(rowid, request_id, trace_id, body, reply_body) VALUES (new.rowid, new.id, new.trace_id, new.body, '');
 END;
 CREATE TRIGGER replies_search_insert AFTER INSERT ON replies BEGIN
   UPDATE requests_fts SET reply_body = new.body WHERE rowid = (SELECT rowid FROM requests WHERE id = new.request_id);
 END;
 CREATE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);
 `)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// backfillSearchBatch commits the index and its checkpoint together. Triggers
// may already have indexed rows beyond the checkpoint; replacing them is safe.
func (s *Store) backfillSearchBatch() (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var high, end int64
	if err := tx.QueryRow(`SELECT high_water FROM search_backfill WHERE id = 1`).Scan(&high); err != nil {
		return false, err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), ?) FROM
 (SELECT rowid FROM requests WHERE rowid > ? ORDER BY rowid LIMIT ?)`, high, high, searchBackfillBatchSize).Scan(&end); err != nil {
		return false, err
	}
	if end == high {
		return false, tx.Commit()
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO requests_fts(rowid, request_id, trace_id, body, reply_body)
 SELECT r.rowid, r.id, r.trace_id, r.body, COALESCE(p.body, '') FROM requests r
 LEFT JOIN replies p ON p.request_id = r.id WHERE r.rowid > ? AND r.rowid <= ?`, high, end); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE search_backfill SET high_water = ? WHERE id = 1`, end); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Search finds request and reply text, newest requests first. An empty
// participant searches everything; otherwise only its chains are visible.
func (s *Store) Search(ctx context.Context, query, participant string, limit int) ([]envelope.SearchResult, error) {
	out := []envelope.SearchResult{}
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	if len(terms) == 0 {
		return out, nil
	}
	for i, term := range terms {
		terms[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 50)
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.trace_id, r.from_agent, r.to_agent, r.status, r.created_at,
 c.snippet, c.reply_snippet, r.attachments, COALESCE(p.attachments, '')
 FROM (
 SELECT rowid,
 CASE WHEN highlight(requests_fts, 2, '[', ']') != body THEN snippet(requests_fts, 2, '[', ']', '…', 24) ELSE '' END AS snippet,
 CASE WHEN highlight(requests_fts, 3, '[', ']') != reply_body THEN snippet(requests_fts, 3, '[', ']', '…', 24) ELSE '' END AS reply_snippet
 FROM requests_fts
 WHERE requests_fts MATCH ? AND (? = '' OR rowid IN (
   -- An agent searches only its own chains, newest searchVisibleWindow
   -- requests first. Every step here is indexed and proportional to the
   -- agent's own history, so matches it cannot see are never examined.
   SELECT r.rowid FROM requests r WHERE r.trace_id IN (
     SELECT trace_id FROM requests WHERE from_agent = ?
     UNION SELECT trace_id FROM requests WHERE to_agent = ?
   ) ORDER BY r.rowid DESC LIMIT ?
 ))
 ORDER BY rowid DESC LIMIT ?
 ) c JOIN requests r ON r.rowid = c.rowid
 LEFT JOIN replies p ON p.request_id = r.id
 ORDER BY r.created_at DESC, r.rowid DESC LIMIT ?`, strings.Join(terms, " AND "), participant, participant, participant, searchVisibleWindow, searchCandidateLimit, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hit envelope.SearchResult
		var created int64
		var requestAttachments, replyAttachments string
		if err := rows.Scan(&hit.RequestID, &hit.TraceID, &hit.From, &hit.To, &hit.Status, &created, &hit.Snippet, &hit.ReplySnippet, &requestAttachments, &replyAttachments); err != nil {
			return nil, err
		}
		hit.CreatedAt = time.UnixMilli(created).UTC()
		for _, raw := range []string{requestAttachments, replyAttachments} {
			attachments, err := decodeAttachments(raw)
			if err != nil {
				return nil, err
			}
			for _, attachment := range attachments {
				hit.AttachmentNames = append(hit.AttachmentNames, attachment.Name)
			}
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}
