package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// DiscoverSession records a locally observed native conversation. The first
// project observed for an agent/host/session remains its identity if the
// process later changes its working directory.
func (s *Store) DiscoverSession(ctx context.Context, binding envelope.SessionBinding) (envelope.SessionBinding, bool, error) {
	if err := validateSessionBinding(binding, false); err != nil {
		return envelope.SessionBinding{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.SessionBinding{}, false, err
	}
	defer tx.Rollback()

	project := binding.Project
	var original string
	err = tx.QueryRowContext(ctx, `SELECT project FROM native_session_discoveries WHERE agent=? AND host=? AND session_id=?`, binding.Agent, binding.Host, binding.SessionID).Scan(&original)
	if err == nil {
		project = original
	} else if !errors.Is(err, sql.ErrNoRows) {
		return envelope.SessionBinding{}, false, err
	}
	binding.Project = project

	var pinnedProject, pinnedSession string
	pinErr := tx.QueryRowContext(ctx, `SELECT project, session_id FROM native_session_pins WHERE agent=? AND host=?`, binding.Agent, binding.Host).Scan(&pinnedProject, &pinnedSession)
	if pinErr != nil && !errors.Is(pinErr, sql.ErrNoRows) {
		return envelope.SessionBinding{}, false, pinErr
	}
	changed := false
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO native_session_discoveries(agent,host,session_id,project,created_at) VALUES(?,?,?,?,?)`, binding.Agent, binding.Host, binding.SessionID, project, time.Now().UnixMilli()); err != nil {
			return envelope.SessionBinding{}, false, err
		}
		changed = true
	}
	if pinErr == nil && pinnedSession == binding.SessionID {
		if pinnedProject != "" && pinnedProject != project {
			return envelope.SessionBinding{}, false, ErrWrongState
		}
		ownerChanged, err := putSessionOwner(ctx, tx, binding, project)
		if err != nil {
			return envelope.SessionBinding{}, false, err
		}
		changed = changed || ownerChanged
		if pinnedProject == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE native_session_pins SET project=? WHERE agent=? AND host=? AND project='' AND session_id=?`, project, binding.Agent, binding.Host, binding.SessionID); err != nil {
				return envelope.SessionBinding{}, false, err
			}
			changed = true
		}
	}
	if err := tx.Commit(); err != nil {
		return envelope.SessionBinding{}, false, err
	}
	return binding, changed, nil
}

// ResolveProjectSession returns a pinned owner, or binds the only discovered
// conversation for the project. It never picks between multiple candidates.
func (s *Store) ResolveProjectSession(ctx context.Context, agent, host, project string) (envelope.SessionBinding, bool, error) {
	probe := envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: "probe"}
	if err := validateSessionBinding(probe, false); err != nil {
		return envelope.SessionBinding{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return envelope.SessionBinding{}, false, err
	}
	defer tx.Rollback()

	var owner string
	err = tx.QueryRowContext(ctx, `SELECT session_id FROM native_session_owners WHERE agent=? AND host=? AND project=?`, agent, host, project).Scan(&owner)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return envelope.SessionBinding{}, false, err
		}
		return envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: owner}, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return envelope.SessionBinding{}, false, err
	}

	var pinProject, pinSession string
	pinErr := tx.QueryRowContext(ctx, `SELECT project, session_id FROM native_session_pins WHERE agent=? AND host=?`, agent, host).Scan(&pinProject, &pinSession)
	if pinErr != nil && !errors.Is(pinErr, sql.ErrNoRows) {
		return envelope.SessionBinding{}, false, pinErr
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id FROM native_session_discoveries WHERE agent=? AND host=? AND project=? ORDER BY session_id`, agent, host, project)
	if err != nil {
		return envelope.SessionBinding{}, false, err
	}
	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return envelope.SessionBinding{}, false, err
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return envelope.SessionBinding{}, false, err
	}
	rows.Close()
	if pinErr == nil && (pinProject == "" || pinProject == project) {
		found := false
		for _, id := range sessions {
			if id == pinSession {
				found = true
				break
			}
		}
		if !found {
			if pinProject == "" {
				return envelope.SessionBinding{}, false, ErrSessionOwnerPreserved
			}
			if len(sessions) == 0 {
				return envelope.SessionBinding{}, false, ErrSessionOwnerPreserved
			}
			return envelope.SessionBinding{}, false, ErrWrongState
		}
		binding := envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: pinSession}
		changed, err := putSessionOwner(ctx, tx, binding, project)
		if err != nil {
			return envelope.SessionBinding{}, false, err
		}
		if pinProject == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE native_session_pins SET project=? WHERE agent=? AND host=? AND project='' AND session_id=?`, project, agent, host, pinSession); err != nil {
				return envelope.SessionBinding{}, false, err
			}
			changed = true
		}
		if err := tx.Commit(); err != nil {
			return envelope.SessionBinding{}, false, err
		}
		return binding, changed, nil
	}
	if len(sessions) == 0 {
		return envelope.SessionBinding{}, false, ErrSessionOwnerMissing
	}
	if len(sessions) > 1 {
		return envelope.SessionBinding{}, false, ErrSessionOwnerAmbiguous
	}
	binding := envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: sessions[0]}
	if pinErr == nil && pinProject == project && pinSession != binding.SessionID {
		return envelope.SessionBinding{}, false, ErrWrongState
	}
	changed, err := putSessionOwner(ctx, tx, binding, project)
	if err != nil {
		return envelope.SessionBinding{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return envelope.SessionBinding{}, false, err
	}
	return binding, changed, nil
}

// RegisteredSession resolves an exact recipient-owned native ID. It fails
// closed for unknown, cross-agent, or multiply registered routes.
func (s *Store) RegisteredSession(ctx context.Context, agent, sessionID string) (envelope.SessionBinding, error) {
	if !identity.ValidName(agent) || !envelope.ValidSessionID(sessionID) || sessionID == "" {
		return envelope.SessionBinding{}, fmt.Errorf("invalid native session lookup")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT host, project FROM native_session_discoveries WHERE agent=? AND session_id=? ORDER BY host, project`, agent, sessionID)
	if err != nil {
		return envelope.SessionBinding{}, err
	}
	defer rows.Close()
	var found []envelope.SessionBinding
	for rows.Next() {
		binding := envelope.SessionBinding{Agent: agent, SessionID: sessionID}
		if err := rows.Scan(&binding.Host, &binding.Project); err != nil {
			return envelope.SessionBinding{}, err
		}
		found = append(found, binding)
	}
	if err := rows.Err(); err != nil {
		return envelope.SessionBinding{}, err
	}
	if len(found) == 0 {
		return envelope.SessionBinding{}, ErrSessionOwnerMissing
	}
	if len(found) != 1 {
		return envelope.SessionBinding{}, ErrSessionOwnerAmbiguous
	}
	return found[0], nil
}

// RetireSession removes one authenticated agent's deleted native
// conversation from discovery and ownership. Active work prevents retirement
// so a lifecycle event can never orphan an in-flight request.
func (s *Store) RetireSession(ctx context.Context, binding envelope.SessionBinding) (bool, error) {
	if !identity.ValidName(binding.Agent) || !envelope.ValidSessionHost(binding.Host) ||
		binding.SessionID == "" || !envelope.ValidSessionID(binding.SessionID) {
		return false, fmt.Errorf("invalid native session retirement")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM requests WHERE to_agent=? AND to_session=? AND status IN (?,?,?,?,?) AND expires_at>?`,
		binding.Agent, binding.SessionID,
		string(envelope.StatusHeld), string(envelope.StatusQueued), string(envelope.StatusDelivered),
		string(envelope.StatusClaimed), string(envelope.StatusNeedsInput), s.now().UnixMilli()).Scan(&active)
	if err != nil {
		return false, err
	}
	if active != 0 {
		return false, ErrSessionHasActiveWork
	}
	var changed bool
	for _, statement := range []string{
		`DELETE FROM native_session_owners WHERE agent=? AND host=? AND session_id=?`,
		`DELETE FROM native_session_pins WHERE agent=? AND host=? AND session_id=?`,
		`DELETE FROM native_session_discoveries WHERE agent=? AND host=? AND session_id=?`,
	} {
		result, err := tx.ExecContext(ctx, statement, binding.Agent, binding.Host, binding.SessionID)
		if err != nil {
			return false, err
		}
		if n, err := result.RowsAffected(); err != nil {
			return false, err
		} else if n > 0 {
			changed = true
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

// PreserveSessionOwner pins a session known before native discovery. An empty
// project leaves the pin unresolved until that same session is observed.
func (s *Store) PreserveSessionOwner(ctx context.Context, binding envelope.SessionBinding) (bool, error) {
	if err := validateSessionBinding(binding, true); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var pinnedProject, pinnedSession string
	pinErr := tx.QueryRowContext(ctx, `SELECT project, session_id FROM native_session_pins WHERE agent=? AND host=?`, binding.Agent, binding.Host).Scan(&pinnedProject, &pinnedSession)
	if pinErr != nil && !errors.Is(pinErr, sql.ErrNoRows) {
		return false, pinErr
	}

	// An unknown-project legacy owner is the only per-host pin. Reimporting it
	// after a matching native event materializes the project remains idempotent.
	if binding.Project == "" {
		if pinErr == nil {
			if pinnedSession != binding.SessionID {
				return false, ErrWrongState
			}
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO native_session_pins(agent,host,project,session_id,created_at) VALUES(?,?,'',?,?)`, binding.Agent, binding.Host, binding.SessionID, time.Now().UnixMilli()); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}

	// Explicit project imports belong in the per-project owner table, so one
	// host can preserve owners for any number of projects.
	var discovered string
	err = tx.QueryRowContext(ctx, `SELECT project FROM native_session_discoveries WHERE agent=? AND host=? AND session_id=?`, binding.Agent, binding.Host, binding.SessionID).Scan(&discovered)
	if err == nil && discovered != binding.Project {
		return false, ErrWrongState
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	materializePin := false
	if pinErr == nil {
		if pinnedProject == binding.Project && pinnedSession != binding.SessionID {
			return false, ErrWrongState
		}
		materializePin = pinnedProject == "" && pinnedSession == binding.SessionID
	}
	changed, err := putSessionOwner(ctx, tx, binding, binding.Project)
	if err != nil {
		return false, err
	}
	if materializePin {
		if _, err := tx.ExecContext(ctx, `UPDATE native_session_pins SET project=? WHERE agent=? AND host=? AND project='' AND session_id=?`, binding.Project, binding.Agent, binding.Host, binding.SessionID); err != nil {
			return false, err
		}
		changed = true
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func putSessionOwner(ctx context.Context, tx *sql.Tx, binding envelope.SessionBinding, project string) (bool, error) {
	var current string
	err := tx.QueryRowContext(ctx, `SELECT session_id FROM native_session_owners WHERE agent=? AND host=? AND project=?`, binding.Agent, binding.Host, project).Scan(&current)
	if err == nil {
		if current != binding.SessionID {
			return false, ErrWrongState
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_owners(agent,host,project,session_id,created_at) VALUES(?,?,?,?,?)`, binding.Agent, binding.Host, project, binding.SessionID, time.Now().UnixMilli())
	return err == nil, err
}

func validateSessionBinding(binding envelope.SessionBinding, allowEmptyProject bool) error {
	if !identity.ValidName(binding.Agent) || !envelope.ValidSessionHost(binding.Host) || binding.SessionID == "" || !envelope.ValidSessionID(binding.SessionID) {
		return fmt.Errorf("invalid native session binding")
	}
	if binding.Project == "" && allowEmptyProject {
		return nil
	}
	if !envelope.ValidSessionProject(binding.Project) {
		return fmt.Errorf("invalid native session project")
	}
	return nil
}

// NativeSessionHosts advertises a host only when authenticated discoveries
// agree. An ambiguous or undiscovered agent gets no inferred native adapter.
func (s *Store) NativeSessionHosts(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent,MIN(host) FROM native_session_discoveries GROUP BY agent HAVING COUNT(DISTINCT host)=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var agent, host string
		if err := rows.Scan(&agent, &host); err != nil {
			return nil, err
		}
		if envelope.ValidSessionHost(host) {
			out[agent] = host
		}
	}
	return out, rows.Err()
}
