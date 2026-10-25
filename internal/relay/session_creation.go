package relay

import (
	"errors"
	"net/http"
	"sync"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// Unlike lookup, this explicit POST may provision one native conversation.
// The operator provisioner must journal uncertain native creation outcomes;
// a timeout must never cause it to blindly create another conversation.
func (s *Server) handleEnsureProjectSession(w http.ResponseWriter, r *http.Request) {
	origin := s.agent(w, r)
	if origin == "" {
		return
	}
	if envelope.Session(r.Context()) == "" {
		writeErr(w, 400, errors.New("originating session required"))
		return
	}
	var in struct {
		Agent   string `json:"agent"`
		Host    string `json:"host"`
		Project string `json:"project"`
	}
	if !decodeSessionCatalogBody(w, r, &in) {
		return
	}
	if in.Agent == "" || !validCatalogScope(in.Host, in.Project) {
		writeErr(w, 400, errors.New("invalid project session scope"))
		return
	}
	if _, ok, err := s.dir.Agent(r.Context(), in.Agent); err != nil || !ok {
		writeErr(w, 404, errors.New("recipient not enrolled"))
		return
	}
	key := in.Agent + "\x00" + in.Host + "\x00" + in.Project
	value, _ := s.projectCreationLocks.LoadOrStore(key, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	binding, _, err := s.store.ResolveProjectSession(r.Context(), in.Agent, in.Host, in.Project)
	created := false
	if errors.Is(err, store.ErrSessionOwnerMissing) {
		if s.cfg.CreateProjectSession == nil {
			writeErr(w, http.StatusServiceUnavailable, errors.New("native session creation is not configured; no task sent"))
			return
		}
		_, reserveErr := s.store.ReserveSessionCreation(r.Context(), in.Agent, in.Host, in.Project)
		if reserveErr != nil {
			writeErr(w, 503, errors.New("creation intent could not be persisted; no task sent"))
			return
		}
		// The operator provisioner owns the native creation journal. Calling its
		// ensure operation again can recover a verified receipt or retry a failed
		// preflight; it must never repeat an uncertain native creation.
		binding, err = s.cfg.CreateProjectSession(r.Context(), in.Agent, in.Host, in.Project)
		if err == nil && (binding.Agent != in.Agent || binding.Host != in.Host || binding.Project != in.Project || !envelope.ValidSessionID(binding.SessionID) || binding.SessionID == "") {
			err = errors.New("native provisioner identity mismatch")
		}
		if err == nil {
			_, _, err = s.store.DiscoverSession(r.Context(), binding)
		}
		if err == nil {
			expectedID := binding.SessionID
			binding, _, err = s.store.ResolveProjectSession(r.Context(), in.Agent, in.Host, in.Project)
			if err == nil && binding.SessionID != expectedID {
				err = errors.New("native creation conflicts with existing project owner; no task sent")
			}
			created = err == nil
		}
	}
	if err != nil {
		writeSessionCatalogError(w, err)
		return
	}
	if created {
		s.record(r.Context(), "project_session_created", "", "", origin, store.DetailJSON(map[string]any{"agent": binding.Agent, "host": binding.Host, "project": binding.Project, "native_session_id": binding.SessionID}))
	}
	writeJSON(w, 200, map[string]any{"binding": binding, "created": created})
}
