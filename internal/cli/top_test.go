package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestAttention(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		agent client.AgentInfo
		score int
		flags string
	}{
		{"healthy", client.AgentInfo{Online: true, Version: "1.2.0"}, 0, ""},
		{"offline", client.AgentInfo{Queued: 1, Wake: "none"}, 8, "QUEUED-OFFLINE"},
		{"wakeable", client.AgentInfo{Queued: 1, Wake: "webhook"}, 0, ""},
		{"stale", client.AgentInfo{Online: true, Queued: 1, OldestQueued: now.Add(-time.Hour - time.Second)}, 4, "STALE"},
		{"boundary", client.AgentInfo{Online: true, Queued: 1, OldestQueued: now.Add(-time.Hour)}, 0, ""},
		{"overdue", client.AgentInfo{Wake: "schedule", Target: envelope.Target{Overdue: true}}, 2, "OVERDUE"},
		{"old", client.AgentInfo{Version: "1.1.0"}, 1, "OLD BUILD"},
		{"new", client.AgentInfo{Version: "1.3.0"}, 0, ""},
		{"claims", client.AgentInfo{Claimed: 2}, 0, "CLAIMED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score, flags := attention(tc.agent, "1.2.0", now)
			if score != tc.score || strings.Join(flags, "|") != tc.flags {
				t.Fatalf("got %d %v", score, flags)
			}
		})
	}
}

func TestRenderFrame(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := topFrame{at: at, roster: client.Roster{RelayVersion: "1.2.0", Agents: []client.AgentInfo{{Name: "alpha", Online: true, Wake: "none", Version: "1.2.0"}, {Name: "zeta", Wake: "none", Queued: 2}}}, note: "held and recent chains need an admin device"}
	want := "tincan top | relay 1.2.0 | online 1/2 | queued 2 | held ? | 12:00:00\n" +
		"AGENT            STATE   WAKE         QUEUED OLDEST CLAIMS VERSION\n" +
		"zeta             offline none              2      -      0 \n" +
		"  QUEUED-OFFLINE\n" +
		"alpha            online  none              0      -      0 1.2.0\n" +
		"held and recent chains need an admin device\n"
	if got := renderFrame(f, 120); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if f.roster.Agents[0].Name != "alpha" {
		t.Fatal("render mutated roster")
	}
	f.admin = true
	f.note = ""
	f.held = []envelope.Request{{ID: "req1", From: "alpha", To: "zeta", Body: "approve me"}}
	f.chains = []envelope.Result{{Request: envelope.Request{CreatedAt: at, From: "alpha", To: "zeta", Body: "hello"}, Status: envelope.StatusQueued}}
	got := renderFrame(f, 120)
	suffix := "Held for approval (1):\n  req1 alpha -> zeta approve me\nRecent chains:\n  12:00:00 alpha -> zeta [queued] hello\n"
	if !strings.HasSuffix(got, suffix) || !strings.Contains(got, "held 1") {
		t.Fatal(got)
	}
	f.held[0].Body = "\x1b[2J\r\n" + strings.Repeat("界", 80)
	for _, width := range []int{1, 40, 80} {
		for line := range strings.SplitSeq(renderFrame(f, width), "\n") {
			if topTestDisplayWidth(line) > width || strings.ContainsAny(line, "\x1b\r") {
				t.Fatalf("unsafe line %q", line)
			}
		}
	}
}

func TestTopOnceMesh(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	// dot-web requests are held by default.
	if err := m.Dir.SetKind(t.Context(), "100.0.0.1:1", "muse", "dot-web"); err != nil {
		t.Fatal(err)
	}
	held, err := m.Client(t, "grokbot").Ask(t.Context(), "muse", "owner review", "", 0, false)
	if err != nil || held.Status != envelope.StatusHeld {
		t.Fatalf("seed held: %+v %v", held, err)
	}
	queued, err := m.Client(t, "grokbot").Ask(t.Context(), "instinct", "queued work", "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	useConfig(t, client.Config{Relay: m.URL("grokbot"), Agent: "grokbot"})
	for _, args := range [][]string{{"--once", "--relay", m.URL("admin")}, {"--relay", m.URL("admin")}, {"--once", "--socket", adminSocket(t, m)}} {
		out, err := run(t, topCmd(), args...)
		if err != nil || !strings.Contains(out, "owner review") || !strings.Contains(out, "Recent chains:") || strings.Contains(out, "\x1b") {
			t.Fatalf("%q %v", out, err)
		}
	}
	out, err := run(t, topCmd(), "--once")
	if err != nil || !strings.Contains(out, "held and recent chains need an admin device") || strings.Contains(out, "owner review") {
		t.Fatalf("nonadmin %q %v", out, err)
	}
	for id, status := range map[string]envelope.Status{held.Request.ID: envelope.StatusHeld, queued.Request.ID: envelope.StatusQueued} {
		res, err := m.Client(t, "grokbot").Get(t.Context(), id, 0)
		if err != nil || res.Status != status {
			t.Fatalf("changed request: %+v %v", res, err)
		}
	}
	if _, err := run(t, topCmd(), "--interval", "999ms"); err == nil {
		t.Fatal("accepted short interval")
	}
}

func TestTopFetchRoutes(t *testing.T) {
	for _, code := range []int{200, 403, 404, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.Method != "GET" {
					t.Errorf("mutation: %s", r.Method)
				}
				switch r.URL.Path {
				case "/v1/agents":
					fmt.Fprint(w, `{"agents":[]}`)
				case "/v1/admin/held":
					w.WriteHeader(code)
					if code == 200 {
						fmt.Fprint(w, `[]`)
					} else {
						fmt.Fprint(w, `{"error":"unavailable"}`)
					}
				case "/v1/trace":
					fmt.Fprint(w, `{"traces":[]}`)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			f, err := fetchFrame(t.Context(), r)
			if (err != nil) != (code == 500) {
				t.Fatalf("error %v", err)
			}
			want := "/v1/agents,/v1/admin/held"
			if code == 200 {
				want += ",/v1/trace?limit=8&exclude_pings=true"
			}
			if strings.Join(paths, ",") != want || f.admin != (code == 200) {
				t.Fatalf("paths=%v admin=%v", paths, f.admin)
			}
		})
	}
}

func topTestDisplayWidth(s string) int {
	cells := 0
	for _, r := range s {
		cells += topRuneWidth(r)
	}
	return cells
}

func TestTopTextDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		text  string
		cells int
	}{
		{"界", 2}, {"Ａ", 2}, {"😀", 2}, {"e\u0301", 1}, {"a", 1}, {"\u200d", 0},
	} {
		if got := topTestDisplayWidth(tc.text); got != tc.cells {
			t.Fatalf("width of %q = %d, want %d", tc.text, got, tc.cells)
		}
	}
	for _, input := range []string{strings.Repeat("界", 60), strings.Repeat("😀", 60), strings.Repeat("e\u0301", 60)} {
		for _, width := range []int{1, 2, 3, 4, 20} {
			got := topText(input, width)
			if topTestDisplayWidth(got) > width {
				t.Fatalf("width %d: overflowing text %q", width, got)
			}
		}
	}
	if got := topText("e\u0301界😀", 5); got != "e\u0301界😀" {
		t.Fatalf("truncated fitting text: %q", got)
	}
}

func TestTopLiveFrameHeight(t *testing.T) {
	f := topFrame{admin: true, roster: client.Roster{Agents: []client.AgentInfo{
		{Name: "healthy", Online: true}, {Name: "urgent", Queued: 1},
	}}}
	for i := range 30 {
		f.held = append(f.held, envelope.Request{ID: fmt.Sprintf("held-%02d", i)})
	}
	f.chains = []envelope.Result{{Request: envelope.Request{Body: "last-chain"}}}
	plain := renderFrame(f, 120)
	all := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	for _, height := range []int{1, 4, 8, len(all), len(all) + 1} {
		got := topLiveFrame(plain, 120, height)
		lines := strings.Split(got, "\n")
		if len(lines) > height || strings.HasSuffix(got, "\n") {
			t.Fatalf("height %d: unbounded frame %q", height, got)
		}
		if len(all) > height {
			want := fmt.Sprintf("... %d more lines (run tincan top --once for all)", len(all)-height+1)
			if lines[len(lines)-1] != want || strings.Join(lines[:height-1], "\n") != strings.Join(all[:height-1], "\n") {
				t.Fatalf("height %d: wrong overflow or order: %q", height, got)
			}
		} else if got != strings.TrimSuffix(plain, "\n") {
			t.Fatalf("fitting frame changed: %q", got)
		}
	}
	if !strings.Contains(topLiveFrame(plain, 120, 4), "urgent") || strings.Contains(topLiveFrame(plain, 120, 4), "healthy") {
		t.Fatal("highest-attention agent not prioritized")
	}
	if !strings.Contains(plain, "held-29") || !strings.Contains(plain, "last-chain") || !strings.HasSuffix(plain, "\n") {
		t.Fatal("plain snapshot incomplete")
	}
	for line := range strings.SplitSeq(topLiveFrame(renderFrame(f, 20), 20, 2), "\n") {
		if topTestDisplayWidth(line) > 20 {
			t.Fatalf("overflow indicator too wide: %q", line)
		}
	}
}
