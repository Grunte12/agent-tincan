package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// wakesMesh runs a relay whose real waker wakes grokbot through a webhook
// with a token in its URL and a bearer token, and instinct by email.
func wakesMesh(t *testing.T) (*testrelay.Mesh, *wake.Waker) {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(hook.Close)
	w := wake.New(wake.Config{
		"grokbot":  {Method: wake.Webhook, URL: hook.URL + "/hooks/URLPATHSECRET?token=URLQUERYSECRET", BearerToken: "BEARERSECRET"},
		"instinct": {Method: wake.Email, EmailTo: "owner@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "EMAILKEYSECRET"},
	}, m.Store, wake.Options{
		HTTP: hook.Client(), AgentMailAPI: hook.URL, Debounce: time.Millisecond, RetryDelay: time.Millisecond,
		WakeGrace: -1, ReplyRetries: []time.Duration{}, Queued: m.Server.QueuedCount, LastPoll: m.Server.LastPoll,
	})
	t.Cleanup(w.Stop)
	m.Server.SetWakeNamer(w)
	m.Server.SetEvents(w)
	useConfig(t, client.Config{Relay: m.URL("admin")})
	return m, w
}

func wakeAgent(t *testing.T, m *testrelay.Mesh, w *wake.Waker, to string) {
	t.Helper()
	if _, err := m.Client(t, "muse").Send(t.Context(), to, "check the garage", envelope.KindAsk, "", false); err != nil {
		t.Fatal(err)
	}
	w.Flush()
}

func TestWakesCLITable(t *testing.T) {
	m, w := wakesMesh(t)
	since := time.Now().Add(-time.Minute).Format(time.RFC3339)
	// An older woke row, from before the status code was recorded. The next
	// wake comes before any activity, so it shows no poll.
	if err := m.Store.Audit(t.Context(), store.AuditEvent{Event: "woke", Actor: "grokbot", Detail: "webhook, 1 waiting"}); err != nil {
		t.Fatal(err)
	}
	wakeAgent(t, m, w, "grokbot")
	if _, err := m.Client(t, "grokbot").Poll(t.Context(), 0); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, Root(), "wakes", "grokbot", "--since", since)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "TIME") || !strings.Contains(lines[0], "NEXT POLL") {
		t.Fatalf("table = %q", out)
	}
	if older := lines[1]; !strings.Contains(older, "woke") || !strings.Contains(older, "2xx") || !strings.Contains(older, "not recorded") || !strings.Contains(older, "no poll") {
		t.Errorf("older row = %q", older)
	}
	if current := lines[2]; !strings.Contains(current, "webhook") || !strings.Contains(current, " 200 ") || !strings.Contains(current, "not recorded") || strings.Contains(current, "no poll") {
		t.Errorf("current row = %q", current)
	}
}

func TestWakesCLINoPollAndEmptyWindow(t *testing.T) {
	m, w := wakesMesh(t)
	since := time.Now().Add(-time.Minute).Format(time.RFC3339)
	wakeAgent(t, m, w, "grokbot")
	out, err := run(t, Root(), "wakes", "grokbot", "--since", since)
	if err != nil || !strings.Contains(out, "no poll") {
		t.Fatalf("unpolled wake = %q, %v", out, err)
	}
	out, err = run(t, Root(), "wakes", "grokbot", "--since", "2020-01-01", "--until", "2020-01-02 00:00")
	if err != nil || !strings.Contains(out, "No wakes for grokbot") {
		t.Fatalf("empty window = %q, %v", out, err)
	}
	out, err = run(t, Root(), "wakes", "grokbot", "--since", "2020-01-01", "--until", "2020-01-02", "--json")
	var empty relay.WakeExport
	if err != nil || json.Unmarshal([]byte(out), &empty) != nil || empty.Wakes == nil || len(empty.Wakes) != 0 {
		t.Fatalf("empty JSON = %q, %v", out, err)
	}
}

func TestWakesCLIJSONCarriesNoSecrets(t *testing.T) {
	m, w := wakesMesh(t)
	wakeAgent(t, m, w, "grokbot")
	wakeAgent(t, m, w, "instinct")
	for _, agent := range []string{"grokbot", "instinct"} {
		out, err := run(t, Root(), "wakes", agent, "--since", "1h", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var got relay.WakeExport
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Wakes) != 1 || got.Wakes[0].Status != "200" {
			t.Fatalf("%s JSON = %s, %v", agent, out, err)
		}
		for _, secret := range []string{"SECRET", "http://", "https://", "/hooks", "token="} {
			if strings.Contains(out, secret) {
				t.Errorf("%s JSON leaks %q: %s", agent, secret, out)
			}
		}
	}
}

func TestWakesCLIErrors(t *testing.T) {
	m, _ := wakesMesh(t)
	if _, err := run(t, Root(), "wakes", "grokbot"); err == nil {
		t.Fatal("no --since accepted")
	}
	if _, err := run(t, Root(), "wakes", "grokbot", "--since", "last tuesday"); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("bad --since = %v", err)
	}
	if _, err := run(t, Root(), "wakes", "grokbot", "--since", "1h", "--relay", m.URL("grokbot")); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("non-admin = %v, want 403", err)
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-10-05T18:00:00Z":      time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC),
		"2026-10-05T11:00:00-07:00": time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC),
		"2026-10-05 11:00":          time.Date(2026, 10, 5, 11, 0, 0, 0, time.Local),
		"2026-10-05":                time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local),
		"90m":                       now.Add(-90 * time.Minute),
	} {
		got, err := parseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}
