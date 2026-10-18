package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

type sessionDiscoveryInput struct {
	Host    string `json:"host"`
	Project string `json:"project"`
}

type sessionRetirementInput struct {
	Host      string `json:"host"`
	SessionID string `json:"session_id"`
}

func decodeSessionCatalogBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid session catalog body"))
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		writeErr(w, http.StatusBadRequest, errors.New("invalid session catalog body"))
		return false
	}
	return true
}

func validCatalogScope(host, project string) bool {
	return envelope.ValidSessionHost(host) && envelope.ValidSessionProject(project)
}

func (s *Server) handleDiscoverSession(w http.ResponseWriter, r *http.Request) {
	agent := s.agent(w, r)
	if agent == "" {
		return
	}
	if envelope.Session(r.Context()) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("session ID required"))
		return
	}
	var in sessionDiscoveryInput
	if !decodeSessionCatalogBody(w, r, &in) {
		return
	}
	if !validCatalogScope(in.Host, in.Project) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid session catalog scope"))
		return
	}
	binding, changed, err := s.store.DiscoverSession(r.Context(), envelope.SessionBinding{
		Agent: agent, Host: in.Host, Project: in.Project, SessionID: envelope.Session(r.Context()),
	})
	if err != nil {
		writeSessionCatalogError(w, err)
		return
	}
	if changed {
		s.record(r.Context(), "session_discovered", "", "", agent, store.DetailJSON(map[string]any{"host": binding.Host, "project": binding.Project, "native_session_id": binding.SessionID}))
	}
	writeJSON(w, http.StatusOK, map[string]any{"binding": binding, "changed": changed})
}

func (s *Server) handleResolveProjectSession(w http.ResponseWriter, r *http.Request) {
	origin := s.agent(w, r)
	if origin == "" {
		return
	}
	if envelope.Session(r.Context()) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("session ID required"))
		return
	}
	q := r.URL.Query()
	if len(q) != 3 || len(q["agent"]) != 1 || len(q["host"]) != 1 || len(q["project"]) != 1 {
		writeErr(w, http.StatusBadRequest, errors.New("agent, host and project are required"))
		return
	}
	target, host, project := q.Get("agent"), q.Get("host"), q.Get("project")
	if target == "" || !validCatalogScope(host, project) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid project session scope"))
		return
	}
	if _, ok, err := s.dir.Agent(r.Context(), target); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	} else if !ok {
		writeErr(w, http.StatusNotFound, identity.ErrUnknownAgent)
		return
	}
	binding, changed, err := s.store.ResolveProjectSession(r.Context(), target, host, project)
	if errors.Is(err, store.ErrSessionOwnerMissing) && s.cfg.DefaultSessionRoutes[target] != "" {
		binding, err = s.mainReceiver(r.Context(), target, project)
		changed = false
	}
	if err != nil {
		writeSessionCatalogError(w, err)
		return
	}
	if changed {
		s.record(r.Context(), "project_owner_bound", "", "", origin, store.DetailJSON(map[string]any{"agent": target, "host": host, "project": project, "native_session_id": binding.SessionID}))
	}
	writeJSON(w, http.StatusOK, map[string]any{"binding": binding, "changed": changed})
}

func (s *Server) handleRetireSession(w http.ResponseWriter, r *http.Request) {
	agent := s.agent(w, r)
	if agent == "" {
		return
	}
	if envelope.Session(r.Context()) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("originating session ID required"))
		return
	}
	var in sessionRetirementInput
	if !decodeSessionCatalogBody(w, r, &in) {
		return
	}
	if !envelope.ValidSessionHost(in.Host) || in.SessionID == "" || !envelope.ValidSessionID(in.SessionID) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid session retirement"))
		return
	}
	changed, err := s.store.RetireSession(r.Context(), envelope.SessionBinding{
		Agent: agent, Host: in.Host, SessionID: in.SessionID,
	})
	if err != nil {
		writeSessionCatalogError(w, err)
		return
	}
	if changed {
		s.record(r.Context(), "session_retired", "", "", agent, store.DetailJSON(map[string]any{"host": in.Host, "native_session_id": in.SessionID}))
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (s *Server) handlePreserveSessionOwner(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(r) {
		writeErr(w, http.StatusForbidden, errors.New("admin device required"))
		return
	}
	var binding envelope.SessionBinding
	if !decodeSessionCatalogBody(w, r, &binding) {
		return
	}
	if binding.Agent == "" || !envelope.ValidSessionID(binding.SessionID) || !envelope.ValidSessionHost(binding.Host) || (binding.Project != "" && !validCatalogScope(binding.Host, binding.Project)) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid session owner binding"))
		return
	}
	if !s.isAgent(r.Context(), binding.Agent) {
		writeErr(w, http.StatusNotFound, identity.ErrUnknownAgent)
		return
	}
	changed, err := s.store.PreserveSessionOwner(r.Context(), binding)
	if err != nil {
		writeSessionCatalogError(w, err)
		return
	}
	if changed {
		s.record(r.Context(), "session_owner_preserved", "", "", "operator", store.DetailJSON(map[string]any{"agent": binding.Agent, "host": binding.Host, "project": binding.Project, "native_session_id": binding.SessionID}))
	}
	writeJSON(w, http.StatusOK, map[string]any{"binding": binding, "changed": changed})
}

func writeSessionCatalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrSessionOwnerAmbiguous), errors.Is(err, store.ErrSessionOwnerPreserved), errors.Is(err, store.ErrSessionHasActiveWork), errors.Is(err, store.ErrWrongState):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrSessionOwnerMissing), errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}
