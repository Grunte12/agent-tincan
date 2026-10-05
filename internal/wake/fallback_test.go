package wake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

// hits records, in order, which wake path each request reached. A path in
// fail answers 502.
type hits struct {
	mu   sync.Mutex
	seq  []string
	fail map[string]bool
}

func (h *hits) server(t *testing.T, label string) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		h.mu.Lock()
		h.seq = append(h.seq, label)
		fail := h.fail[label]
		h.mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"status":"queued"}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (h *hits) order() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seq...)
}

func (h *hits) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.order()) < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.order(); len(got) < n {
		t.Fatalf("wake sends = %v, want at least %d", got, n)
	}
}

// emailPath is an AgentMail fallback; the test points AgentMailAPI at its
// server.
func emailPath() Target {
	return Target{Method: Email, EmailTo: "bot@example.com", AgentMailFrom: "tincan@agentmail.to", AgentMailKey: "am-key-secret"}
}

// details lists the audit detail of each event named event, oldest first.
func details(t *testing.T, st *store.Store, event string) []string {
	t.Helper()
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Event == event {
			out = append(out, e.Detail)
		}
	}
	return out
}

func waitEvent(t *testing.T, st *store.Store, event string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(details(t, st, event)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if len(details(t, st, event)) == 0 {
		t.Fatalf("no %s event; events = %v", event, events(t, st))
	}
}

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// Each silent follow-up moves to the next path, and after the last fallback
// it comes back to the primary.
func TestFallbackFollowUpsCycleThroughPaths(t *testing.T) {
	var h hits
	primary, second, mail := h.server(t, "primary"), h.server(t, "fallback1"), h.server(t, "email")
	st := auditStore(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, Fallback: []Target{{Method: Webhook, URL: second.URL}, emailPath()}}}
	w := New(cfg, st, Options{
		Debounce:     time.Millisecond,
		WakeGrace:    20 * time.Millisecond,
		AgentMailAPI: mail.URL,
		Queued:       func(string) int { return int(queuedN.Load()) },
		LastPoll:     func(string) time.Time { return time.Time{} },
		Now:          fixedClock(time.Unix(1_790_000_000, 0)),
	})
	queued(w, "grokbot", 1)
	h.wait(t, 4)
	drainFollowUp(t, w, &queuedN)
	if got, want := strings.Join(h.order()[:4], ","), "primary,fallback1,email,primary"; got != want {
		t.Fatalf("send order = %s, want %s", got, want)
	}
	woke := details(t, st, "woke")
	for i, want := range []string{"webhook, HTTP 200, 1 waiting", "fallback 1: webhook, HTTP 200, 1 waiting", "fallback 2: email, HTTP 200, 1 waiting", "webhook, HTTP 200, 1 waiting"} {
		if !strings.HasPrefix(woke[i], want) {
			t.Errorf("woke %d detail = %q, want prefix %q", i, woke[i], want)
		}
	}
	for _, d := range woke {
		if strings.Contains(d, primary.URL) || strings.Contains(d, second.URL) || strings.Contains(d, "am-key-secret") || strings.Contains(d, "bot@example.com") {
			t.Errorf("woke detail %q names a URL, address or key", d)
		}
	}
}

// Every path draws on the agent's one max_per_hour, and the stored result
// marks the fallback that sent last.
func TestFallbackSharesHourlyBudget(t *testing.T) {
	var h hits
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	st := auditStore(t)
	start := time.Unix(1_790_000_000, 0)
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, MaxPerHour: 2, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, st, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
		Now:       fixedClock(start),
	})
	queued(w, "grokbot", 1)
	waitEvent(t, st, "wake_skipped")
	drainFollowUp(t, w, &queuedN)
	if got := strings.Join(h.order(), ","); got != "primary,fallback1" {
		t.Fatalf("sends = %s, want primary,fallback1 then the shared cap", got)
	}
	want := store.Wake{At: start, Result: "ok (fallback 1: webhook)"}
	if got, ok := w.LastWake("grokbot"); !ok || !got.At.Equal(want.At) || got.Result != want.Result {
		t.Fatalf("last wake = %+v, want %+v", got, want)
	}
	stored, err := st.LastWakes(context.Background())
	if err != nil || stored["grokbot"].Result != want.Result {
		t.Fatalf("stored last wake = %+v %v, want result %q", stored["grokbot"], err, want.Result)
	}
	if !envelope.WakeAccepted(want.Result) {
		t.Fatalf("%q should read as an accepted wake", want.Result)
	}
}

// A failed send still moves the next follow-up to the next path, and the
// failure on a fallback is marked with it.
func TestFallbackFailedSendAdvances(t *testing.T) {
	h := hits{fail: map[string]bool{"primary": true, "fallback1": true}}
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	st := auditStore(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, MaxPerHour: 2, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        fixedClock(time.Unix(1_790_000_000, 0)),
	})
	queued(w, "grokbot", 1)
	waitEvent(t, st, "wake_skipped")
	drainFollowUp(t, w, &queuedN)
	// Each path gets its send and the one retry.
	if got := strings.Join(h.order(), ","); got != "primary,primary,fallback1,fallback1" {
		t.Fatalf("sends = %s", got)
	}
	failed := details(t, st, "wake_failed")
	if len(failed) != 2 || strings.Contains(failed[0], "fallback") || !strings.HasSuffix(failed[1], "returned 502 Bad Gateway (fallback 1: webhook)") {
		t.Fatalf("wake_failed details = %q", failed)
	}
	if got, _ := w.LastWake("grokbot"); got.Result != failed[1] || envelope.WakeAccepted(got.Result) {
		t.Fatalf("last wake result = %q, want %q", got.Result, failed[1])
	}
}

// A poll ends the episode: the next wake starts again on the primary.
func TestFallbackPollResetsToPrimary(t *testing.T) {
	var h hits
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	start := time.Unix(1_790_000_000, 0)
	var now, poll atomicTime
	now.set(start)
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 50 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return poll.get() },
		Now:       now.get,
	})
	queued(w, "grokbot", 1)
	h.wait(t, 2)
	poll.set(start)
	w.Flush()
	if got := strings.Join(h.order(), ","); got != "primary,fallback1" {
		t.Fatalf("sends before the poll = %s", got)
	}
	now.set(start.Add(time.Minute))
	queued(w, "grokbot", 1)
	h.wait(t, 3)
	drainFollowUp(t, w, &queuedN)
	if got := h.order()[2]; got != "primary" {
		t.Fatalf("first wake after a poll went to %s, want primary (sends %v)", got, h.order())
	}
}

// Without fallback, follow-ups resend the one path and the result stays a
// plain "ok", as before fallbacks existed.
func TestNoFallbackResendsPrimary(t *testing.T) {
	var h hits
	primary := h.server(t, "primary")
	st := auditStore(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	w := New(Config{"grokbot": {Method: Webhook, URL: primary.URL}}, st, Options{
		Debounce:  time.Millisecond,
		WakeGrace: 20 * time.Millisecond,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return time.Time{} },
		Now:       fixedClock(time.Unix(1_790_000_000, 0)),
	})
	queued(w, "grokbot", 1)
	h.wait(t, 3)
	drainFollowUp(t, w, &queuedN)
	for _, d := range details(t, st, "woke") {
		if !strings.HasPrefix(d, "webhook, HTTP 200, 1 waiting") || strings.Contains(d, "fallback") {
			t.Errorf("woke detail = %q", d)
		}
	}
	if got, _ := w.LastWake("grokbot"); got.Result != envelope.WakeOK {
		t.Fatalf("last wake result = %q, want ok", got.Result)
	}
}

func TestLoadConfigFallback(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wake.json")
	good := `{"grokbot":{"method":"webhook","url":"http://x","max_per_hour":6,"fallback":[
		{"method":"email","email_to":"a@b","agentmail_inbox":"c@d","agentmail_key":"k"},
		{"method":"webhook","url":"http://y","bearer_token":"t"}]}}`
	os.WriteFile(p, []byte(good), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fb := c["grokbot"].Fallback; len(fb) != 2 || fb[0].Method != Email || fb[1].BearerToken != "t" {
		t.Fatalf("fallback = %+v", fb)
	}
	for _, tc := range []struct{ cfg, want string }{
		{`{"muse":{"method":"wait","fallback":[{"method":"webhook","url":"http://y"}]}}`, `wake muse: fallback needs a webhook or email primary, not "wait"`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"wait"}]}}`, `wake grokbot: fallback 1: method must be webhook or email, not "wait"`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"webhook","url":"http://y"},{"method":"webhook"}]}}`, `wake grokbot: fallback 2: webhook needs url`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"email","email_to":"a@b"}]}}`, `wake grokbot: fallback 1: email needs email_to, agentmail_inbox, agentmail_key`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"webhook","url":"http://y","fallback":[{"method":"webhook","url":"http://z"}]}]}}`, `wake grokbot: fallback 1: a fallback cannot have its own fallback list`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"webhook","url":"http://y","fallback":[]}]}}`, `wake grokbot: fallback 1: a fallback cannot have its own fallback list`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"webhook","url":"http://y","max_per_hour":3}]}}`, `wake grokbot: fallback 1: max_per_hour belongs on the agent`},
		{`{"grokbot":{"method":"webhook","url":"http://x","fallback":[{"method":"webhook","url":"http://y","format":"openclaw"}]}}`, `wake grokbot: fallback 1: format openclaw needs bearer_token`},
	} {
		os.WriteFile(p, []byte(tc.cfg), 0o600)
		if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("config %s: err = %v, want %q", tc.cfg, err, tc.want)
		}
	}
}

// lockedBuffer is a log sink safe for the waker's timer goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The 2xx response body reaches the relay log and the woke audit detail as
// one short line with every credential and the URL redacted; the stored
// result, which agents can read, stays "ok".
func TestWebhookAnswerSummarySanitized(t *testing.T) {
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// A careless platform echoing what it was sent.
		w.Write([]byte("{\"status\":\"queued\",\n\t\"echo\":\"" + r.URL.String() + "\",\x1b[31m\"auth\":\"" + r.Header.Get("Authorization") +
			"\",\"sig\":\"" + r.Header.Get("X-Hub-Signature-256") + "\"}"))
	}))
	t.Cleanup(ts.Close)
	st := auditStore(t)
	url := ts.URL + "/hooks/path-secret?token=query-secret"
	w := New(Config{"grokbot": {Method: Webhook, URL: url, BearerToken: "bearer-secret", HMACSecret: "hmac-secret"}}, st, Options{Debounce: time.Millisecond, WakeGrace: skipFollowUp})
	queued(w, "grokbot", 1)
	w.Flush()
	woke := details(t, st, "woke")
	if len(woke) != 1 || !strings.Contains(woke[0], `response: {"status":"queued", "echo":"[redacted]`) {
		t.Fatalf("woke detail = %q", woke)
	}
	m := hmac.New(sha256.New, []byte("hmac-secret"))
	m.Write(webhookBody(Target{}, Message(1)))
	sig := hex.EncodeToString(m.Sum(nil))
	for _, out := range []string{woke[0], logged.String()} {
		for _, leak := range []string{"secret", sig, "\x1b", "\t"} {
			if strings.Contains(out, leak) {
				t.Errorf("%q leaks %q", out, leak)
			}
		}
	}
	if !strings.Contains(logged.String(), "wake grokbot: ok, webhook, HTTP 200, 1 waiting, response: ") {
		t.Errorf("relay log = %q", logged.String())
	}
	if got, _ := w.LastWake("grokbot"); got.Result != envelope.WakeOK {
		t.Fatalf("last wake result = %q, want only ok", got.Result)
	}
}

func TestAnswerSummary(t *testing.T) {
	long := strings.Repeat("a", 195) + "SECRETTOKEN" + strings.Repeat("b", 50)
	for _, tc := range []struct {
		name, raw string
		secrets   []string
		want      string
	}{
		{"empty", "", nil, ""},
		{"one line", "{\"ok\": true}\r\n\r\n", nil, `{"ok": true}`},
		{"controls and bidi", "a\x00b\u202ec\x7fd", nil, "a b c d"},
		{"json-escaped url", `{"u":"https:\/\/h\/hook\/abcd"}`, []string{"https://h/hook/abcd"}, `{"u":"[redacted]"}`},
		{"bare path kept", `{"p":"/"}`, []string{"/", ""}, `{"p":"/"}`},
		{"short secret withholds the body", `{"echo":"k9x"}`, []string{"k9x"}, "[withheld: may contain a secret]"},
		{"short secret absent", `{"ok":true}`, []string{"k9x"}, `{"ok":true}`},
		{"secret at the cut", long, []string{"SECRETTOKEN"}, strings.Repeat("a", 195) + "[reda..."},
		{"rune boundary", strings.Repeat("a", 199) + "é" + "z", nil, strings.Repeat("a", 199) + "..."},
		{"invalid utf8", "ok\xff", nil, "ok?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := answerSummary([]byte(tc.raw), tc.secrets); got != tc.want {
				t.Fatalf("answerSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// A request that arrives while a silent agent's follow-up is due rides that
// follow-up, which moves to the next path, instead of resending the path
// that did not start the agent.
func TestFallbackNewRequestKeepsFollowUp(t *testing.T) {
	var h hits
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	st := auditStore(t)
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, MaxPerHour: 2, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, st, Options{
		Debounce:   time.Millisecond,
		RetryDelay: time.Millisecond,
		WakeGrace:  time.Second,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return time.Time{} },
		Now:        fixedClock(time.Unix(1_790_000_000, 0)),
	})
	queued(w, "grokbot", 1)
	// Wait until the first wake has gone out and its follow-up is due.
	followUpDue := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := w.pending["grokbot"]
		return p != nil && p.followUp
	}
	for deadline := time.Now().Add(5 * time.Second); !followUpDue(); {
		if time.Now().After(deadline) {
			t.Fatal("no follow-up after the first wake")
		}
		time.Sleep(time.Millisecond)
	}
	queuedN.Store(2)
	queued(w, "grokbot", 1)
	waitEvent(t, st, "wake_skipped")
	drainFollowUp(t, w, &queuedN)
	if got := strings.Join(h.order(), ","); got != "primary,fallback1" {
		t.Fatalf("sends = %s, want primary,fallback1", got)
	}
}

// followUpDue reports whether agent's request follow-up is armed.
func followUpDue(w *Waker, agent string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pending[agent]
	return p != nil && p.followUp
}

func waitFollowUpDue(t *testing.T, w *Waker, agent string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !followUpDue(w, agent); {
		if time.Now().After(deadline) {
			t.Fatal("no follow-up after the first wake")
		}
		time.Sleep(time.Millisecond)
	}
}

// A request that arrives after the agent checked in is woken for at once,
// on the primary, even with an old follow-up still armed.
func TestNewRequestAfterPollWakesAtOnce(t *testing.T) {
	var h hits
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	start := time.Unix(1_790_000_000, 0)
	var poll atomicTime
	var queuedN atomic.Int32
	queuedN.Store(1)
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, nil, Options{
		Debounce:  time.Millisecond,
		WakeGrace: time.Hour,
		Queued:    func(string) int { return int(queuedN.Load()) },
		LastPoll:  func(string) time.Time { return poll.get() },
		Now:       fixedClock(start),
	})
	t.Cleanup(w.Stop)
	queued(w, "grokbot", 1)
	waitFollowUpDue(t, w, "grokbot")
	poll.set(start.Add(time.Second)) // the agent checked in
	queuedN.Store(2)
	queued(w, "grokbot", 1)
	h.wait(t, 2)
	if got := strings.Join(h.order(), ","); got != "primary,primary" {
		t.Fatalf("sends = %s, want primary,primary", got)
	}
}

// A request that rode a silent agent's follow-up still gets a wake when the
// agent checks in before the follow-up fires: a fresh one on the primary,
// and the owner is not told of an unanswered wake.
func TestRiddenFollowUpTurnsFreshAfterPoll(t *testing.T) {
	var h hits
	primary, second := h.server(t, "primary"), h.server(t, "fallback1")
	start := time.Unix(1_790_000_000, 0)
	var poll atomicTime
	var queuedN atomic.Int32
	queuedN.Store(1)
	var unanswered atomic.Int32
	cfg := Config{"grokbot": {Method: Webhook, URL: primary.URL, Fallback: []Target{{Method: Webhook, URL: second.URL}}}}
	w := New(cfg, nil, Options{
		Debounce:   time.Millisecond,
		WakeGrace:  300 * time.Millisecond,
		Queued:     func(string) int { return int(queuedN.Load()) },
		LastPoll:   func(string) time.Time { return poll.get() },
		Now:        fixedClock(start),
		Unanswered: func(string, store.Wake) { unanswered.Add(1) },
	})
	t.Cleanup(w.Stop)
	queued(w, "grokbot", 1)
	waitFollowUpDue(t, w, "grokbot")
	queuedN.Store(2)
	queued(w, "grokbot", 1) // the agent is silent: this rides the follow-up
	if got := len(h.order()); got != 1 {
		t.Fatalf("sends after the ridden request = %d, want 1", got)
	}
	poll.set(start.Add(time.Second))
	queuedN.Store(1) // the poll took the first request
	w.Flush()
	if got := strings.Join(h.order(), ","); got != "primary,primary" {
		t.Fatalf("sends = %s, want primary,primary", got)
	}
	if n := unanswered.Load(); n != 0 {
		t.Fatalf("unanswered told %d times after the agent checked in", n)
	}
}

// A restarted relay carries on from the path its last stored wake used.
func TestRestartKeepsFallbackPosition(t *testing.T) {
	st := auditStore(t)
	at := time.Unix(1_790_000_000, 0)
	if err := st.SetLastWake(context.Background(), "grokbot", store.Wake{At: at, Result: "ok (fallback 1: webhook)"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastWake(context.Background(), "hermes", store.Wake{At: at, Result: "ok (fallback 3: email)"}); err != nil {
		t.Fatal(err)
	}
	two := []Target{{Method: Webhook, URL: "http://b"}, {Method: Webhook, URL: "http://c"}}
	w := New(Config{"grokbot": {Method: Webhook, URL: "http://a", Fallback: two}, "hermes": {Method: Webhook, URL: "http://a", Fallback: two}}, st, Options{})
	t.Cleanup(w.Stop)
	if got := w.step["grokbot"]; got != 1 {
		t.Fatalf("grokbot step after restart = %d, want 1", got)
	}
	// A path the config no longer has starts again on the primary.
	if got := w.step["hermes"]; got != 0 {
		t.Fatalf("hermes step after restart = %d, want 0", got)
	}
	if got := w.pathStep("grokbot", true); got != 2 {
		t.Fatalf("next follow-up path = %d, want 2", got)
	}
}

// A secret a webhook echoes inside a JSON string, where quotes, backslashes
// and HTML characters are escaped, is still redacted.
func TestAnswerSummaryRedactsJSONEscapedSecret(t *testing.T) {
	const sec = `tok"en\with</x>&more`
	bodies := []string{
		`{"echo":"tok\"en\\with\u003c/x\u003e\u0026more"}`,
		`{"echo":"tok\"en\\with</x>&more"}`,
		`{"echo":"tok\"en\\with<\/x>&more"}`,
		`{"echo":"tok\"en\\with\u003c\/x\u003e\u0026more"}`,
		`{"echo":"` + sec + `"}`,
	}
	for _, b := range bodies {
		got := answerSummary([]byte(b), []string{sec})
		if got != `{"echo":"[redacted]"}` {
			t.Errorf("answerSummary(%s) = %q, want the secret redacted", b, got)
		}
	}
}
