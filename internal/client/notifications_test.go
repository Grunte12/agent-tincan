package client_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestNotificationsDropsSessionScopeWhileClaimAndGetKeepIt(t *testing.T) {
	const configuredSession = "00000000-0000-4000-8000-000000000001"
	const runtimeSession = "00000000-0000-4000-8000-000000000002"
	type call struct{ path, agent, session string }
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, call{r.URL.Path, r.Header.Get(client.AgentHeader), r.Header.Get(client.SessionHeader)})
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/notifications":
			fmt.Fprint(w, `{"waiting":1,"queued":1,"notifications":[{"id":"ask-1","session_id":"00000000-0000-4000-8000-000000000003","kind":"request"}]}`)
		case "/v1/capabilities":
			fmt.Fprint(w, `{"session_routing":true}`)
		case "/v1/requests/ask-1/claim", "/v1/requests/ask-1":
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r, err := client.NewRelayForFile(client.Config{Relay: srv.URL, Agent: "codex", SessionID: configuredSession}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := envelope.WithSession(t.Context(), runtimeSession)
	w, err := r.Notifications(ctx, 0)
	if err != nil || len(w.Notifications) != 1 || w.Notifications[0].SessionID != "00000000-0000-4000-8000-000000000003" {
		t.Fatalf("notifications = %+v, %v", w, err)
	}
	mu.Lock()
	got := append([]call(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0].path != "/v1/notifications" || got[0].agent != "codex" || got[0].session != "" {
		t.Fatalf("aggregate request headers/calls = %+v", got)
	}
	if _, err := r.Claim(ctx, "ask-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "ask-1", 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got = append([]call(nil), calls...)
	mu.Unlock()
	for _, path := range []string{"/v1/requests/ask-1/claim", "/v1/requests/ask-1"} {
		found := false
		for _, c := range got {
			if c.path == path {
				found = true
				if c.agent != "codex" || c.session != runtimeSession {
					t.Errorf("%s headers = %+v; want authenticated scoped call", path, c)
				}
			}
		}
		if !found {
			t.Errorf("missing %s call in %+v", path, got)
		}
	}
	for _, c := range got {
		if c.path == "/v1/capabilities" && c.session == "" {
			t.Errorf("scoped capability check lost its session: %+v", c)
		}
	}
}
