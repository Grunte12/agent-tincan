package wake

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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

type recorder struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	auth   []string
	sigs   []string
	fail   atomic.Int32 // fail this many requests first
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.bodies = append(rc.bodies, string(raw))
		rc.paths = append(rc.paths, r.URL.Path)
		rc.auth = append(rc.auth, r.Header.Get("Authorization"))
		rc.sigs = append(rc.sigs, r.Header.Get("X-Hub-Signature-256"))
		rc.mu.Unlock()
		if rc.fail.Load() > 0 {
			rc.fail.Add(-1)
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

func queued(w *Waker, to string, n int) {
	for i := range n {
		w.Queued(context.Background(), envelope.Request{ID: "r" + string(rune('a'+i)), To: to, Body: "SECRET call Joe's Garage at 555-0100"})
	}
}

func auditStore(t *testing.T) *store.Store {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func events(t *testing.T, st *store.Store) []string {
	evs, err := st.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

func TestBurstGivesOneWebhookWithoutRequestText(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, st, Options{Debounce: 50 * time.Millisecond})
	queued(w, "grokbot", 5)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if !strings.Contains(body["message"], "5 requests") || strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" {
		t.Fatalf("auth = %q", rc.auth[0])
	}
	if got := strings.Join(events(t, st), ","); got != "woke" {
		t.Fatalf("audit = %s", got)
	}
}

func TestWebhookRetriesOnceThenAuditsFailure(t *testing.T) {
	var rc recorder
	rc.fail.Store(1)
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"grokbot": {Method: Webhook, URL: ts.URL}}, st, Options{Debounce: time.Millisecond, RetryDelay: time.Millisecond})
	queued(w, "grokbot", 1)
	w.Flush()
	if rc.count() != 2 || strings.Join(events(t, st), ",") != "woke" {
		t.Fatalf("calls=%d audit=%v", rc.count(), events(t, st))
	}
	rc.fail.Store(2)
	queued(w, "grokbot", 1)
	w.Flush()
	if got := events(t, st); got[len(got)-1] != "wake_failed" {
		t.Fatalf("audit = %v, want wake_failed last", got)
	}
}

// The generic format carries the count in both message and text, for
// runtimes that read either field.
func TestWebhookBodyMirrorsTextWithBearerOnly(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"openclaw": {Method: Webhook, URL: ts.URL, BearerToken: "tok"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "openclaw", 2)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(rc.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "agent-tincan" || body["message"] != Message(2) || body["text"] != body["message"] {
		t.Fatalf("wake body = %s", rc.bodies[0])
	}
	if rc.auth[0] != "Bearer tok" || rc.sigs[0] != "" {
		t.Fatalf("auth = %q sig = %q", rc.auth[0], rc.sigs[0])
	}
}

// Hermes verifies webhooks with the GitHub HMAC scheme.
func TestWebhookHMACSignsExactBody(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL, HMACSecret: "hush"}}, nil, Options{Debounce: time.Millisecond})
	queued(w, "hermes", 1)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want 1", rc.count())
	}
	sign := func(secret string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(rc.bodies[0]))
		return "sha256=" + hex.EncodeToString(m.Sum(nil))
	}
	if rc.sigs[0] != sign("hush") {
		t.Fatalf("sig = %q, want %q", rc.sigs[0], sign("hush"))
	}
	if hmac.Equal([]byte(rc.sigs[0]), []byte(sign("wrong"))) {
		t.Fatal("signature verified with the wrong secret")
	}
	if rc.auth[0] != "" {
		t.Fatalf("auth = %q, want none", rc.auth[0])
	}
	if strings.Contains(rc.bodies[0], "SECRET") || strings.Contains(rc.bodies[0], "555") || strings.Contains(rc.bodies[0], "hush") {
		t.Fatalf("wake body leaks: %s", rc.bodies[0])
	}
}

// Instinct wakes on email; the relay sends it through Grok Bot's AgentMail.
func TestEmailWakeUsesAgentMail(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"instinct": {Method: Email, EmailTo: "agent@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "am_key"}},
		nil, Options{Debounce: time.Millisecond, AgentMailAPI: ts.URL + "/v0"})
	queued(w, "instinct", 2)
	w.Flush()
	if rc.count() != 1 || rc.paths[0] != "/v0/inboxes/bot@agentmail.to/messages/send" || rc.auth[0] != "Bearer am_key" {
		t.Fatalf("paths=%v auth=%v", rc.paths, rc.auth)
	}
	var body map[string]string
	json.Unmarshal([]byte(rc.bodies[0]), &body)
	if body["to"] != "agent@example.com" || !strings.Contains(body["text"], "2 requests") || strings.Contains(rc.bodies[0], "SECRET") {
		t.Fatalf("email body = %s", rc.bodies[0])
	}
}

func TestBudgetStopsWakesButNotQueueing(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	now := time.Unix(1_790_000_000, 0)
	w := New(Config{"instinct": {Method: Webhook, URL: ts.URL, MaxPerHour: 2}}, st, Options{Debounce: time.Millisecond, Now: func() time.Time { return now }})
	for range 3 {
		queued(w, "instinct", 1)
		w.Flush()
	}
	if rc.count() != 2 {
		t.Fatalf("wakes = %d, want 2", rc.count())
	}
	if got := events(t, st); got[len(got)-1] != "wake_skipped" {
		t.Fatalf("audit = %v", got)
	}
	now = now.Add(61 * time.Minute)
	queued(w, "instinct", 1)
	w.Flush()
	if rc.count() != 3 {
		t.Fatalf("after the hour, wakes = %d, want 3", rc.count())
	}
}

func TestAgentSideMethodsAndOnlineAgentsNeedNoRelayWake(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"muse":        {Method: Wait},
		"claude-code": {Method: Channel},
		"grokbot":     {Method: Webhook, URL: ts.URL},
	}, nil, Options{Debounce: time.Millisecond, Online: func(a string) bool { return a == "grokbot" }})
	queued(w, "muse", 1)
	queued(w, "claude-code", 1)
	queued(w, "grokbot", 1) // online: its poller has it
	queued(w, "chatgpt", 1) // not configured: none
	w.Flush()
	if rc.count() != 0 {
		t.Fatalf("unexpected wakes: %d", rc.count())
	}
	for agent, want := range map[string]string{"muse": Wait, "claude-code": Channel, "grokbot": Webhook, "chatgpt": None} {
		if got := w.WakeMethod(agent); got != want {
			t.Errorf("%s wake = %s, want %s", agent, got, want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	if c, err := LoadConfig(filepath.Join(dir, "missing.json")); err != nil || len(c) != 0 {
		t.Fatalf("missing file: %v %v", c, err)
	}
	p := filepath.Join(dir, "wake.json")
	os.WriteFile(p, []byte(`{"grokbot":{"method":"webhook","url":"http://x"}}`), 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable config should be refused: %v", err)
	}
	os.Chmod(p, 0o600)
	if c, err := LoadConfig(p); err != nil || c["grokbot"].Method != Webhook {
		t.Fatalf("load: %v %v", c, err)
	}
	os.WriteFile(p, []byte(`{"hermes":{"method":"webhook","url":"http://x","hmac_secret":"hush"}}`), 0o600)
	if c, err := LoadConfig(p); err != nil || c["hermes"].HMACSecret != "hush" {
		t.Fatalf("hmac_secret: %v %v", c, err)
	}
	for _, bad := range []string{`{"a":{"method":"webhook"}}`, `{"a":{"method":"email","email_to":"x"}}`, `{"a":{"method":"smoke-signal"}}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("config %s should be rejected", bad)
		}
	}
}

// A requeue happens because the agent's own claim or delivery timed out, so a
// recent poll does not mean a poller is holding the request: wake anyway.
func TestRequeuedWakesEvenWhenRecentlyOnline(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{
		"hermes": {Method: Webhook, URL: ts.URL},
		"muse":   {Method: Wait},
	}, nil, Options{Debounce: time.Millisecond, Online: func(string) bool { return true }})
	w.Requeued(context.Background(), envelope.Request{ID: "r1", To: "hermes"})
	w.Requeued(context.Background(), envelope.Request{ID: "r2", To: "muse"}) // agent-side method: no relay wake
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1 for the requeued webhook agent", rc.count())
	}
}

// A webhook agent counts as online for a few seconds after its last poll, so
// a request queued just as its session ends skips the wake. Nobody is left to
// take it, so the waker checks again later and wakes it if it is still queued.
func TestOnlineSkipRechecksAndWakesWhenStillQueued(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var stillQueued atomic.Int32
	stillQueued.Store(1)
	w := New(Config{"hermes": {Method: Webhook, URL: ts.URL}}, nil, Options{
		Debounce:      time.Millisecond,
		OnlineRecheck: 20 * time.Millisecond,
		Online:        func(string) bool { return true },
		Queued:        func(string) int { return int(stillQueued.Load()) },
	})
	queued(w, "hermes", 1) // session polled a moment ago, then ended
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want 1: the request is still queued after the recheck", rc.count())
	}
	if !strings.Contains(rc.bodies[0], "1") {
		t.Errorf("nudge should count the waiting request: %s", rc.bodies[0])
	}

	// A poller that really held it leaves nothing queued: no wake.
	stillQueued.Store(0)
	queued(w, "hermes", 1)
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("wakes = %d, want still 1 once the poller took the request", rc.count())
	}
}

func TestUrgentImmediateOnlineAndBudget(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	w := New(Config{"target": {Method: Webhook, URL: ts.URL, MaxPerHour: 1}}, st, Options{Debounce: time.Hour, Online: func(string) bool { return true }})
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	done := make(chan struct{})
	go func() { w.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("urgent wake waited for debounce or online recheck")
	}
	if rc.count() != 1 {
		t.Fatalf("wakes = %d", rc.count())
	}
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("urgent exceeded hourly budget: %d", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "woke,wake_skipped" {
		t.Fatalf("audit = %s", got)
	}
}

func TestUrgentPullsPendingWakeForward(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	w := New(Config{"target": {Method: Webhook, URL: ts.URL}}, auditStore(t), Options{Debounce: time.Hour})
	w.Queued(t.Context(), envelope.Request{To: "target"})
	w.Queued(t.Context(), envelope.Request{To: "target", Urgent: true})
	done := make(chan struct{})
	go func() { w.Flush(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("urgent request did not pull in pending wake")
	}
	if rc.count() != 1 {
		t.Fatalf("wakes = %d", rc.count())
	}
}

// Stop drops scheduled nudges, cuts short one waiting to retry, and
// schedules nothing afterwards, so a stopping relay is not held up by its
// wakes.
func TestStopDropsAndEndsNudges(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	st := auditStore(t)
	cfg := Config{"grokbot": {Method: Webhook, URL: ts.URL}, "muse": {Method: Webhook, URL: ts.URL}}
	w := New(cfg, st, Options{Debounce: time.Hour, RetryDelay: time.Hour})
	queued(w, "grokbot", 1) // waits out the hour-long debounce
	rc.fail.Store(1)
	w.Queued(context.Background(), envelope.Request{ID: "ru", To: "muse", Urgent: true}) // fails, then waits an hour to retry
	deadline := time.Now().Add(5 * time.Second)
	for rc.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	w.Stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Stop took %v", took)
	}
	queued(w, "grokbot", 1)
	w.ReplyWaiting("muse")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("webhook calls = %d, want only the failed urgent one", rc.count())
	}
	if got := strings.Join(events(t, st), ","); got != "wake_failed" {
		t.Fatalf("audit = %s, want the cut-short nudge recorded as failed", got)
	}
}

// A restarted relay re-arms the request wakes the old process lost. The
// nudge counts the queued requests when it fires, so one a poller took in
// the meantime wakes nobody, and agents woken by their own side are skipped.
func TestRequestsWaitingCountsAtFireTime(t *testing.T) {
	var rc recorder
	ts := rc.server(t)
	var queuedN atomic.Int32
	queuedN.Store(2)
	cfg := Config{"grokbot": {Method: Webhook, URL: ts.URL}, "muse": {Method: Command}}
	w := New(cfg, nil, Options{Debounce: time.Millisecond, Queued: func(string) int { return int(queuedN.Load()) }})
	w.RequestsWaiting("grokbot")
	w.RequestsWaiting("muse")
	w.Flush()
	if rc.count() != 1 || !strings.Contains(rc.bodies[0], "2 requests") {
		t.Fatalf("wakes = %q, want one for grokbot's 2 requests", rc.bodies)
	}
	queuedN.Store(0) // a poller took them before the nudge fired
	w.RequestsWaiting("grokbot")
	w.Flush()
	if rc.count() != 1 {
		t.Fatalf("woke %d times for requests already taken", rc.count())
	}
}
