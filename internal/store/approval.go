package store

// migrateApproval records approval history independently of lifecycle status.
// The transaction makes the columns and legacy backfill an atomic migration.
func (s *Store) migrateApproval() error {
	var has bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('requests') WHERE name = 'was_held')`).Scan(&has); err != nil {
		return err
	}
	if has {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`ALTER TABLE requests ADD COLUMN was_held INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN approved INTEGER NOT NULL DEFAULT 0`,
		`UPDATE requests SET was_held = 1 WHERE status = 'held' OR id IN
    (SELECT request_id FROM audit WHERE event IN ('held', 'approved', 'denied', 'hold_expired'))`,
		`UPDATE requests SET approved = 1 WHERE id IN
    (SELECT request_id FROM audit WHERE event = 'approved')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
