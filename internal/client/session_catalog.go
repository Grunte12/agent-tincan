package client

import (
	"context"
	"errors"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"net/url"
)

func (r *Relay) catalogReady(ctx context.Context) error {
	if r.sessionScope(ctx) == "" {
		return errors.New("native originating session required for session discovery")
	}
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !caps.SessionCatalog {
		return errors.New("relay session catalog unavailable; no conversation or task created")
	}
	return nil
}

// DiscoverSession records only this authenticated caller's actual native ID.
// It sends no task and never claims or polls work.
func (r *Relay) DiscoverSession(ctx context.Context, host, project string) (envelope.SessionBinding, error) {
	var out struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	if err := r.catalogReady(ctx); err != nil {
		return out.Binding, err
	}
	err := r.call(ctx, r.api, "POST", "/v1/session-discovery", map[string]string{"host": host, "project": project}, &out)
	if err == nil && (out.Binding.Agent != r.agent || out.Binding.Host != host || out.Binding.SessionID != r.sessionScope(ctx) || out.Binding.Project == "") {
		return envelope.SessionBinding{}, errors.New("native discovery identity mismatch")
	}
	return out.Binding, err
}

// ResolveProjectSession reads/binds an existing immutable owner on the relay,
// shared by local and remote runtimes. It never launches a host executor.
func (r *Relay) ResolveProjectSession(ctx context.Context, agent, host, project string) (envelope.SessionBinding, error) {
	var out struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	if err := r.catalogReady(ctx); err != nil {
		return out.Binding, err
	}
	query := url.Values{"agent": {agent}, "host": {host}, "project": {project}}
	err := r.call(ctx, r.api, "GET", "/v1/project-session?"+query.Encode(), nil, &out)
	if err == nil && (out.Binding.Agent != agent || out.Binding.Host != host || out.Binding.Project != project || out.Binding.SessionID == "" || !envelope.ValidSessionID(out.Binding.SessionID)) {
		return envelope.SessionBinding{}, errors.New("project owner identity mismatch")
	}
	return out.Binding, err
}

// RetireSession removes one of this authenticated agent's deleted native
// conversations. It sends no task and cannot retire another agent's route.
func (r *Relay) RetireSession(ctx context.Context, host, sessionID string) (bool, error) {
	if err := r.catalogReady(ctx); err != nil {
		return false, err
	}
	var out struct {
		Changed bool `json:"changed"`
	}
	err := r.call(ctx, r.api, "POST", "/v1/session-retirement", map[string]string{"host": host, "session_id": sessionID}, &out)
	return out.Changed, err
}

// EnsureProjectSession explicitly provisions a missing native project owner on the relay host.
func (r *Relay) EnsureProjectSession(ctx context.Context, agent, host, project string) (envelope.SessionBinding, error) {
	var out struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	if err := r.catalogReady(ctx); err != nil {
		return out.Binding, err
	}
	err := r.call(ctx, r.api, "POST", "/v1/project-session", map[string]string{"agent": agent, "host": host, "project": project}, &out)
	if err == nil && (out.Binding.Agent != agent || out.Binding.Host != host || out.Binding.Project != project || out.Binding.SessionID == "" || !envelope.ValidSessionID(out.Binding.SessionID)) {
		return envelope.SessionBinding{}, errors.New("created project owner identity mismatch")
	}
	return out.Binding, err
}
