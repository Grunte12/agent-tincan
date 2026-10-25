package mcpserver_test

import (
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPAskChecksProjectSessionMatch(t *testing.T) {
	for _, target := range []string{"antigravity", "codex"} {
		for _, mode := range []string{"project", "session", "both", "mismatch", "missing"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				sends := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/v1/capabilities":
						w.Write([]byte(`{"session_routing":true,"session_catalog":true}`))
					case "/v1/agents":
						json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]string{{"name": target, "kind": "generic"}}})
					case "/v1/project-session":
						if mode == "missing" {
							http.Error(w, "owner not found", 404)
							return
						}
						json.NewEncoder(w).Encode(map[string]any{"binding": map[string]string{"agent": target, "host": target, "project": "/new-project", "session_id": "target-owner"}})
					case "/v1/send":
						sends++
						var body map[string]any
						json.NewDecoder(r.Body).Decode(&body)
						if body["session_id"] != "target-owner" || r.Header.Get(client.SessionHeader) != "sender-owner" {
							t.Errorf("wrong scope: %v", body)
						}
						w.Write([]byte(`{"request":{"id":"fixture","status":"answered"}}`))
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				}))
				defer srv.Close()
				t.Setenv("TINCAN_SESSION_HOSTS", `{"antigravity":"antigravity","codex":"codex"}`)
				r, err := client.NewRelayForFile(client.Config{Relay: srv.URL, Agent: "grokbot", SessionID: "sender-owner"}, "")
				if err != nil {
					t.Fatal(err)
				}
				st, ct := mcp.NewInMemoryTransports()
				ss, err := mcpserver.New(r, "test").Connect(t.Context(), st, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer ss.Close()
				cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer cs.Close()
				args := map[string]any{"to": target, "message": "new scoped work", "notify": true}
				if mode != "session" {
					args["project"] = "/new-project"
				}
				if mode == "session" || mode == "both" {
					args["session_id"] = "target-owner"
				}
				if mode == "mismatch" {
					args["session_id"] = "other-owner"
				}
				result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "ask", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				rejected := mode == "mismatch" || mode == "missing"
				if result.IsError != rejected {
					t.Fatalf("error=%v wanted rejection=%v", result.IsError, rejected)
				}
				want := 1
				if rejected {
					want = 0
				}
				if sends != want {
					t.Fatalf("sends=%d wanted=%d", sends, want)
				}
			})
		}
	}
}
