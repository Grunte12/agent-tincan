package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

const catalogProject = "/workspace/project"

func projectSessionURL(agent, host, project string) string {
	q := url.Values{"agent": {agent}, "host": {host}, "project": {project}}
	return "/v1/project-session?" + q.Encode()
}

func (h *harness) catalogDo(addr, agent, session, method, path, body string, want int, out any) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	if agent != "" {
		req.Header.Set(client.AgentHeader, agent)
	}
	if session != "" {
		req.Header.Set(client.SessionHeader, session)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			h.t.Fatalf("decode response: %v: %s", err, rec.Body.String())
		}
	}
	return rec
}

func TestSessionCatalogHTTPDiscoveryAndResolution(t *testing.T) {
	h := newHarness(t, Config{})
	const sid = "native-session-01"
	h.catalogDo(museAddr, "muse", sid, "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/workspace/project"}`, http.StatusOK, nil)
	var resolved struct {
		Binding envelope.SessionBinding `json:"binding"`
		Changed bool                    `json:"changed"`
	}
	h.catalogDo(grokAddr, "grokbot", "origin-session", "GET", "/v1/project-session?agent=muse&host=antigravity&project=%2Fworkspace%2Fproject", "", http.StatusOK, &resolved)
	if resolved.Binding.Agent != "muse" || resolved.Binding.SessionID != sid || resolved.Binding.Project != catalogProject || !resolved.Changed {
		t.Fatalf("resolved binding = %+v changed=%v", resolved.Binding, resolved.Changed)
	}
	events, err := h.st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Event == "session_discovered" {
			found = true
			if strings.Contains(event.Detail, "transcript") || strings.Contains(event.Detail, "token") || !strings.Contains(event.Detail, "antigravity") || !strings.Contains(event.Detail, `"native_session_id":"`+sid+`"`) {
				t.Fatalf("unsafe/incomplete discovery audit: %s", event.Detail)
			}
		}
	}
	if !found {
		t.Fatal("session_discovered audit missing")
	}
	var queued int
	if err := h.st.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("catalog changed queue: %d rows", queued)
	}
}

func TestSessionCatalogHTTPRejectsIdentityOverrideAndBadScope(t *testing.T) {
	h := newHarness(t, Config{})
	const sid = "native-session-02"
	h.catalogDo(museAddr, "muse", sid, "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/workspace/project","agent":"grokbot"}`, http.StatusBadRequest, nil)
	h.catalogDo(museAddr, "muse", "", "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/workspace/project"}`, http.StatusBadRequest, nil)
	h.catalogDo(museAddr, "muse", sid, "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/workspace/project"}`, http.StatusOK, nil)
	// The authenticated node cannot claim another enrolled agent's identity.
	h.catalogDo(museAddr, "grokbot", "native-session-03", "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/workspace/project"}`, http.StatusForbidden, nil)
	h.catalogDo(grokAddr, "grokbot", "origin-session", "GET", "/v1/project-session?agent=missing&host=antigravity&project=%2Fworkspace%2Fproject", "", http.StatusNotFound, nil)
}

func TestSessionCatalogPreservedOwnerRouteIsAdminOnlyAndMigrates(t *testing.T) {
	h := newHarness(t, Config{})
	binding := `{"agent":"muse","host":"muse","project":"","session_id":"old-session-01"}`
	h.catalogDo(grokAddr, "grokbot", "", "POST", "/v1/admin/session-owners/preserve", binding, http.StatusForbidden, nil)
	h.catalogDo(macAddr, "", "", "POST", "/v1/admin/session-owners/preserve", binding, http.StatusOK, nil)
	if rec := h.catalogDo(grokAddr, "grokbot", "origin-session", "GET", "/v1/project-session?agent=muse&host=muse&project=%2Fworkspace%2Fproject", "", http.StatusConflict, nil); !strings.Contains(rec.Body.String(), store.ErrSessionOwnerPreserved.Error()) {
		t.Fatalf("preserved owner error = %s", rec.Body.String())
	}
	h.catalogDo(museAddr, "muse", "old-session-01", "POST", "/v1/session-discovery", `{"host":"muse","project":"/workspace/project"}`, http.StatusOK, nil)
	var resolved struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	h.catalogDo(grokAddr, "grokbot", "origin-session", "GET", "/v1/project-session?agent=muse&host=muse&project=%2Fworkspace%2Fproject", "", http.StatusOK, &resolved)
	if resolved.Binding.SessionID != "old-session-01" {
		t.Fatalf("preserved owner migrated to %q", resolved.Binding.SessionID)
	}
}

func TestClaudeSessionDiscoveryResolutionAndRetirement(t *testing.T) {
	h := newHarness(t, Config{RequireSessionRoutingTo: []string{"claude-code"}})
	h.joinAt(strangerAddr, "claude-code")
	const sid = "00000000-0000-4000-8000-000000000321"
	h.catalogDo(strangerAddr, "claude-code", sid, "POST", "/v1/session-discovery",
		`{"host":"claude-code","project":"/workspace/video"}`, http.StatusOK, nil)
	var resolved struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	h.catalogDo(grokAddr, "grokbot", "grok-session", "GET",
		projectSessionURL("claude-code", "claude-code", "/workspace/video"), "", http.StatusOK, &resolved)
	if resolved.Binding.SessionID != sid {
		t.Fatalf("resolved Claude route = %+v", resolved.Binding)
	}
	h.catalogDo(strangerAddr, "claude-code", "different-live-session", "POST", "/v1/session-retirement",
		`{"host":"claude-code","session_id":"`+sid+`"}`, http.StatusOK, nil)
	rec := h.catalogDo(grokAddr, "grokbot", "grok-session", "POST", "/v1/send",
		`{"to":"claude-code","kind":"ask","body":"must not queue","session_id":"`+sid+`"}`,
		http.StatusNotFound, nil)
	if !strings.Contains(rec.Body.String(), store.ErrSessionOwnerMissing.Error()) {
		t.Fatalf("retired route rejection = %s", rec.Body.String())
	}
}
