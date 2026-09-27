package store

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

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
		return tx.Commit()
	}
	_, err = tx.Exec(`
 CREATE VIRTUAL TABLE requests_fts USING fts5(request_id UNINDEXED, trace_id UNINDEXED, body, reply_body);
 INSERT INTO requests_fts(rowid, request_id, trace_id, body, reply_body)
 SELECT r.rowid, r.id, r.trace_id, r.body, COALESCE(p.body, '') FROM requests r LEFT JOIN replies p ON p.request_id = r.id;
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
 snippet(requests_fts, -1, '[', ']', '…', 24), r.attachments, COALESCE(p.attachments, '')
 FROM requests_fts JOIN requests r ON r.rowid = requests_fts.rowid
 LEFT JOIN replies p ON p.request_id = r.id
 WHERE requests_fts MATCH ? AND (? = '' OR EXISTS (
   SELECT 1 FROM requests step WHERE step.trace_id = r.trace_id AND (step.from_agent = ? OR step.to_agent = ?)
 )) ORDER BY r.created_at DESC, r.rowid DESC LIMIT ?`, strings.Join(terms, " AND "), participant, participant, participant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hit envelope.SearchResult
		var created int64
		var requestAttachments, replyAttachments string
		if err := rows.Scan(&hit.RequestID, &hit.TraceID, &hit.From, &hit.To, &hit.Status, &created, &hit.Snippet, &requestAttachments, &replyAttachments); err != nil {
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
