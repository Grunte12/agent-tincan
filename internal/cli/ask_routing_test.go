package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestAskRoutesProjectAndSessionWithoutChangingSender(t *testing.T) {
	for _, target := range []string{"antigravity", "codex"} {
		for _, mode := range []string{"project", "session", "both", "mismatch"} {
			for _, notify := range []bool{false, true} {
				t.Run(target+"/"+mode+map[bool]string{false: "/ask", true: "/notify"}[notify], func(t *testing.T) {
					sends := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/v1/capabilities":
							w.Write([]byte(`{"session_routing":true,"session_catalog":true}`))
						case "/v1/agents":
							json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]string{{"name": target, "kind": target}}})
						case "/v1/project-session":
							if r.URL.Query().Get("project") != "/Users/grunte/Workspace/repos/nba-analytics" {
								t.Error("wrong project resolution")
							}
							json.NewEncoder(w).Encode(map[string]any{"binding": map[string]string{"agent": target, "host": target, "project": "/Users/grunte/Workspace/repos/nba-analytics", "session_id": catalogTargetSession}})
						case "/v1/send":
							sends++
							var body map[string]any
							json.NewDecoder(r.Body).Decode(&body)
							if body["session_id"] != catalogTargetSession || r.Header.Get(client.SessionHeader) != catalogOriginSession {
								t.Errorf("sender/recipient scopes: %v, %s", body, r.Header.Get(client.SessionHeader))
							}
							if mode != "session" && body["project"] != "/Users/grunte/Workspace/repos/nba-analytics" {
								t.Error("project missing from send")
							}
							w.Write([]byte(`{"request":{"id":"fixture","status":"answered"}}`))
						default:
							t.Errorf("unexpected endpoint %s", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					sessionCatalogConfig(t, server.URL, "grokbot")
					t.Setenv("TINCAN_SESSION_ID", catalogOriginSession)
					t.Setenv("TINCAN_SESSION", catalogOriginSession)
					args := []string{"ask", target, "new scoped work", "--wait", "0s"}
					if mode != "session" {
						args = append(args, "--project", "/Users/grunte/Workspace/repos/nba-analytics")
					} else {
						args = append(args, "--session-id", catalogTargetSession)
					}
					if mode == "both" {
						args = append(args, "--session-id", catalogTargetSession)
					}
					if mode == "mismatch" {
						args = append(args, "--session-id", "wrong-session")
					}
					if notify {
						args = append(args, "--notify")
					}
					_, err := executeSessionCLI(t, args...)
					if mode == "mismatch" {
						if err == nil || sends != 0 {
							t.Fatalf("mismatch err=%v sends=%d", err, sends)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if sends != 1 {
						t.Fatalf("sends=%d", sends)
					}
				})
			}
		}
	}
}

func TestAskRejectsAmbiguousRoutingBeforeSend(t *testing.T) {
	for _, args := range [][]string{
		{"ask", "antigravity,codex", "work", "--session-id", catalogTargetSession},
		{"ask", "antigravity", "work", "--session-id", "invalid/id"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected network request %s", r.URL.Path) }))
			defer server.Close()
			sessionCatalogConfig(t, server.URL, "grokbot")
			if _, err := executeSessionCLI(t, args...); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}
