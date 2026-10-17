package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func TestListenSessionNotificationsFlagUsesAggregateObserver(t *testing.T) {
	const session = "00000000-0000-4000-8000-000000000041"
	var notifications, polls, claims atomic.Int32
	var agentHeader, sessionHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/notifications":
			notifications.Add(1)
			agentHeader, sessionHeader = r.Header.Get(client.AgentHeader), r.Header.Get(client.SessionHeader)
			fmt.Fprint(w, `{"waiting":1,"queued":1,"notifications":[{"id":"ask-1","session_id":"00000000-0000-4000-8000-000000000042","kind":"request"}]}`)
		case "/v1/poll":
			polls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/v1/capabilities":
			fmt.Fprint(w, `{"session_routing":true}`)
		default:
			if strings.HasSuffix(r.URL.Path, "/claim") {
				claims.Add(1)
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "client.json")
	cfg := client.Config{Relay: srv.URL, Agent: "codex", SessionID: session, RelayKey: "relay-key", RelayInfoAt: time.Now()}
	if err := client.SaveConfigTo(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_CONFIG", configPath)
	t.Setenv("TINCAN_SESSION_ID", session)
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")

	marker := filepath.Join(t.TempDir(), "notification.json")
	cmd := listenCmd()
	cmd.SetArgs([]string{"--exec", `printf '%s\n%s' "$TINCAN_WAITING" "$TINCAN_SESSION_NOTIFICATIONS" > ` + fmt.Sprintf("%q", marker), "--once", "--session-notifications"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if notifications.Load() != 1 || polls.Load() != 0 || claims.Load() != 0 {
		t.Fatalf("calls: notifications=%d polls=%d claims=%d", notifications.Load(), polls.Load(), claims.Load())
	}
	if agentHeader != "codex" || sessionHeader != "" {
		t.Fatalf("observer headers: agent=%q session=%q", agentHeader, sessionHeader)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(got), "\n", 2)
	if len(lines) != 2 || lines[0] != "1" || !strings.Contains(lines[1], `"session_id":"00000000-0000-4000-8000-000000000042"`) {
		t.Fatalf("exec environment output = %q", got)
	}
}
