package relay

import (
	"context"
	"errors"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// mainReceiver returns an operator-selected shared receiver. It never
// replaces an existing project owner and never creates or pins a project.
func (s *Server) mainReceiver(ctx context.Context, agent, project string) (envelope.SessionBinding, error) {
	id := s.cfg.DefaultSessionRoutes[agent]
	if id == "" || !envelope.ValidSessionID(id) {
		return envelope.SessionBinding{}, errors.New("invalid main receiver configuration")
	}
	rows, err := s.store.DB().QueryContext(ctx, `SELECT DISTINCT host FROM native_session_discoveries WHERE agent=? AND session_id=? ORDER BY host`, agent, id)
	if err != nil {
		return envelope.SessionBinding{}, err
	}
	defer rows.Close()
	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return envelope.SessionBinding{}, err
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return envelope.SessionBinding{}, err
	}
	if len(hosts) != 1 {
		return envelope.SessionBinding{}, store.ErrSessionOwnerMissing
	}
	host := hosts[0]
	if project != "" {
		binding, _, err := s.store.ResolveProjectSession(ctx, agent, host, project)
		if err == nil {
			return binding, nil
		}
		if !errors.Is(err, store.ErrSessionOwnerMissing) {
			return envelope.SessionBinding{}, err
		}
	}
	return envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: id}, nil
}
