package relay

import (
	"net/http"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestMainReceiverFillsOnlyMissingRoute(t *testing.T) {
	h := newHarness(t, Config{
		DefaultSessionRoutes:    map[string]string{"muse": "muse-main"},
		RequireSessionRoutingTo: []string{"muse"},
	})
	h.catalogDo(museAddr, "muse", "muse-main", "POST", "/v1/session-discovery",
		`{"host":"muse","project":"/workspace/main"}`, http.StatusOK, nil)
	h.catalogDo(museAddr, "muse", "muse-project", "POST", "/v1/session-discovery",
		`{"host":"muse","project":"/workspace/project"}`, http.StatusOK, nil)

	var fallback envelope.Request
	h.catalogDo(grokAddr, "grokbot", "origin", "POST", "/v1/send",
		`{"to":"muse","kind":"ask","body":"new work"}`, http.StatusCreated, &fallback)
	if fallback.ToSession != "muse-main" {
		t.Fatalf("fallback session = %q", fallback.ToSession)
	}

	var explicit envelope.Request
	h.catalogDo(grokAddr, "grokbot", "origin", "POST", "/v1/send",
		`{"to":"muse","kind":"ask","body":"continued work","session_id":"muse-project"}`, http.StatusCreated, &explicit)
	if explicit.ToSession != "muse-project" {
		t.Fatalf("explicit session changed to %q", explicit.ToSession)
	}
}

func TestMainReceiverDoesNotReplaceProjectOwner(t *testing.T) {
	h := newHarness(t, Config{DefaultSessionRoutes: map[string]string{"muse": "muse-main"}})
	h.catalogDo(museAddr, "muse", "muse-main", "POST", "/v1/session-discovery",
		`{"host":"muse","project":"/workspace/main"}`, http.StatusOK, nil)
	h.catalogDo(museAddr, "muse", "muse-project", "POST", "/v1/session-discovery",
		`{"host":"muse","project":"/workspace/project"}`, http.StatusOK, nil)

	var resolved struct {
		Binding envelope.SessionBinding `json:"binding"`
	}
	h.catalogDo(grokAddr, "grokbot", "origin", "GET",
		projectSessionURL("muse", "muse", "/workspace/project"), "", http.StatusOK, &resolved)
	if resolved.Binding.SessionID != "muse-project" {
		t.Fatalf("project owner replaced by %q", resolved.Binding.SessionID)
	}

	h.catalogDo(grokAddr, "grokbot", "origin", "GET",
		projectSessionURL("muse", "muse", "/workspace/new"), "", http.StatusOK, &resolved)
	if resolved.Binding.SessionID != "muse-main" || resolved.Binding.Project != "/workspace/new" {
		t.Fatalf("new project fallback = %+v", resolved.Binding)
	}
}
