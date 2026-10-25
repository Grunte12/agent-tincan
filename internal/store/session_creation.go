package store

import "context"

// ReserveSessionCreation persists intent before launching a native runtime.
// A crashed or timed-out attempt requires reconciliation, never a blind retry.
func (s *Store) ReserveSessionCreation(ctx context.Context, agent, host, project string) (bool, error) {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS native_session_creation_intents (agent TEXT NOT NULL, host TEXT NOT NULL, project TEXT NOT NULL, PRIMARY KEY(agent,host,project))`)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO native_session_creation_intents(agent,host,project) VALUES(?,?,?)`, agent, host, project)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
