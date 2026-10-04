package store

import (
	"context"
	"time"
)

// Wake is the last wake the relay sent an agent: when, and "ok" or the
// error that made the send and its retry fail.
type Wake struct {
	At     time.Time
	Result string
}

// WakeOK is Wake.Result for a send the agent's platform accepted.
const WakeOK = "ok"

// migrateWakes creates the table holding each relay-woken agent's last wake,
// one row per agent. It is a no-op when the table exists.
func (s *Store) migrateWakes() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS wakes (agent TEXT PRIMARY KEY, woken_at INTEGER NOT NULL, result TEXT NOT NULL)`)
	return err
}

// SetLastWake records w as agent's last wake, replacing the one before.
func (s *Store) SetLastWake(ctx context.Context, agent string, w Wake) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO wakes (agent, woken_at, result) VALUES (?, ?, ?)
ON CONFLICT(agent) DO UPDATE SET woken_at = excluded.woken_at, result = excluded.result`, agent, w.At.UnixMilli(), w.Result)
	return err
}

// LastWakes returns the last wake of every agent the relay has woken.
func (s *Store) LastWakes(ctx context.Context) (map[string]Wake, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent, woken_at, result FROM wakes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Wake{}
	for rows.Next() {
		var name, result string
		var ms int64
		if err := rows.Scan(&name, &ms, &result); err != nil {
			return nil, err
		}
		out[name] = Wake{At: time.UnixMilli(ms), Result: result}
	}
	return out, rows.Err()
}
