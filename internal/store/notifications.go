package store

import (
	"context"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const MaxNotifications = 200

type Notification struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	Kind       string `json:"kind"`
	Generation int    `json:"generation,omitempty"`
}

// Notifications returns routing metadata only. It does not deliver, claim,
// acknowledge, or expose the body of any request or reply.
func (s *Store) Notifications(ctx context.Context, agent string) ([]Notification, error) {
	args := []any{agent, string(envelope.StatusQueued), s.now().UnixMilli(), string(envelope.KindPing), agent}
	args = append(args, replyStatuses...)
	args = append(args, MaxNotifications)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, kind, generation FROM (
			SELECT id, to_session AS session_id, 'request' AS kind, 0 AS generation, created_at
			FROM requests
			WHERE to_agent = ? AND to_session != '' AND status = ? AND expires_at > ? AND kind != ?
			UNION ALL
			SELECT q.id, q.from_session AS session_id, 'reply' AS kind, q.reply_generation AS generation, p.created_at
			FROM requests q JOIN replies p ON p.request_id = q.id
			WHERE q.from_agent = ? AND q.from_session != '' AND q.reply_seen_at = 0 AND q.`+replyStatusIn+`
		) ORDER BY created_at, id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Notification, 0)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.SessionID, &n.Kind, &n.Generation); err != nil {
			return nil, err
		}
		if n.SessionID != "" && envelope.ValidSessionID(n.SessionID) {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}
