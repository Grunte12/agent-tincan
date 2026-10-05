package relay

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// notifyPreparer starts chains like the default preparer and names the
// approval policy's notify destination, the owner's agent.
type notifyPreparer struct {
	newChain
	to string
}

func (p notifyPreparer) NotifyDestination() string { return p.to }

// ownerNotices polls addr and returns the owner wake notices it was
// delivered, leaving out anything else (an asker's own wake notice).
func ownerNotices(t *testing.T, h *harness, addr string) []envelope.Request {
	t.Helper()
	var out []envelope.Request
	for _, r := range inbox(t, h, addr) {
		if r.From == "relay" && strings.Contains(r.Body, "tincan wakes") {
			out = append(out, r)
		}
	}
	return out
}

// silentFollowUps calls the waker's Unanswered hook n times for wk, a full
// wake grace apart, as the waker does while grokbot stays silent.
func silentFollowUps(h *harness, clk *fakeClock, wk store.Wake, n int) {
	for range n {
		clk.advance(DefaultWakeGrace)
		h.srv.TellAskers("grokbot", wk)
	}
}

// Three silent follow-ups tell the owner once, naming the agent, the wake
// path, the last wake result and where to read the wake history. More
// follow-ups and a restarted relay do not tell again, and the asker's own
// note on the request is not replaced by the owner's.
func TestOwnerWakeNoticeOncePerEpisode(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	h.srv.SetPreparer(notifyPreparer{to: "muse"})
	ask := h.send(instinctAddr, "grokbot", "book the 3pm slot")
	wk := store.Wake{At: clk.Now(), Result: "hooks.example returned 502 Bad Gateway"}
	fw.set("grokbot", wk)

	silentFollowUps(h, clk, wk, 2)
	if got := ownerNotices(t, h, museAddr); len(got) != 0 {
		t.Fatalf("owner told after two silent follow-ups: %+v", got)
	}
	silentFollowUps(h, clk, wk, 1)
	got := ownerNotices(t, h, museAddr)
	if len(got) != 1 {
		t.Fatalf("owner notices after three silent follow-ups = %+v, want one", got)
	}
	n := got[0]
	if n.Kind != envelope.KindNotify || n.To != "muse" {
		t.Fatalf("owner notice = %+v", n)
	}
	for _, want := range []string{"grokbot has not checked in after 3 wakes in a row", "wake path: webhook", "last wake result: hooks.example returned 502 Bad Gateway", "tincan wakes grokbot"} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("owner notice lacks %q: %s", want, n.Body)
		}
	}

	silentFollowUps(h, clk, wk, 3)
	if got := ownerNotices(t, h, museAddr); len(got) != 0 {
		t.Fatalf("owner told again in the same episode: %+v", got)
	}

	var res envelope.Result
	h.do(instinctAddr, "GET", "/v1/requests/"+ask.ID, "", http.StatusOK, &res)
	if res.RelayNote == nil || strings.Contains(res.RelayNote.Note, "tincan wakes") {
		t.Fatalf("asker's note = %+v, want its own wake note", res.RelayNote)
	}

	// A restarted relay has the same store and does not tell again.
	srv := New(identity.NewDirectory(h.st, h.who, identity.Config{Admins: []string{"macbook-pro-44"}}), h.st, Config{Now: clk.Now})
	srv.SetWakeNamer(fw)
	srv.SetPreparer(notifyPreparer{to: "muse"})
	h2 := &harness{t: t, srv: srv, h: srv.Handler(), st: h.st, who: h.who}
	silentFollowUps(h2, clk, wk, 4)
	if got := ownerNotices(t, h2, museAddr); len(got) != 0 {
		t.Fatalf("owner told again after a restart: %+v", got)
	}
}

// A poll after two silent follow-ups resets the count: the next follow-up
// for that wake is not the third, and nobody tells the owner.
func TestOwnerWakeNoticePollResetsCount(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	h.srv.SetPreparer(notifyPreparer{to: "muse"})
	h.send(instinctAddr, "grokbot", "x")
	wk := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("grokbot", wk)

	silentFollowUps(h, clk, wk, 2)
	h.do(grokAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusOK, nil)
	silentFollowUps(h, clk, wk, 1)
	wk2 := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("grokbot", wk2)
	silentFollowUps(h, clk, wk2, 2)
	if got := ownerNotices(t, h, museAddr); len(got) != 0 {
		t.Fatalf("owner told although grokbot polled: %+v", got)
	}
}

// A new silent episode after a poll tells the owner again.
func TestOwnerWakeNoticeNewEpisode(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	h.srv.SetPreparer(notifyPreparer{to: "muse"})
	h.send(instinctAddr, "grokbot", "x")
	wk := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("grokbot", wk)
	silentFollowUps(h, clk, wk, 3)
	if got := ownerNotices(t, h, museAddr); len(got) != 1 {
		t.Fatalf("first episode owner notices = %+v, want one", got)
	}

	h.do(grokAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusOK, nil)
	clk.advance(time.Minute)
	wk2 := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("grokbot", wk2)
	silentFollowUps(h, clk, wk2, 3)
	got := ownerNotices(t, h, museAddr)
	if len(got) != 1 || !strings.Contains(got[0].Body, "last wake result: ok") {
		t.Fatalf("second episode owner notices = %+v, want one", got)
	}
}

// With no notify destination configured, nothing is queued for an owner,
// nothing is recorded, and the relay logs it once.
func TestOwnerWakeNoticeNoDestination(t *testing.T) {
	h, clk, fw, _ := noticeHarness(t)
	logs, prev := &lockedBuffer{}, log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	h.send(instinctAddr, "grokbot", "x")
	wk := store.Wake{At: clk.Now(), Result: envelope.WakeOK}
	fw.set("grokbot", wk)
	silentFollowUps(h, clk, wk, 5)

	for _, addr := range []string{museAddr, instinctAddr, strangerAddr} {
		if got := ownerNotices(t, h, addr); len(got) != 0 {
			t.Fatalf("owner notice queued with no destination: %+v", got)
		}
	}
	var notes int
	if err := h.st.DB().QueryRow(`SELECT COUNT(*) FROM relay_notes WHERE kind = ?`, store.NoteOwnerWake).Scan(&notes); err != nil || notes != 0 {
		t.Fatalf("owner wake notes = %d, %v; want none", notes, err)
	}
	if n := strings.Count(logs.String(), "no notify destination"); n != 1 {
		t.Fatalf("log lines about the missing destination = %d, want 1:\n%s", n, logs.String())
	}
}

// End to end with the real waker: a webhook that answers 2xx but never
// leads to a poll gets the owner one notice, which carries no webhook URL
// or bearer token.
func TestOwnerWakeNoticeRealWaker(t *testing.T) {
	h := newHarness(t, Config{})
	h.srv.SetPreparer(notifyPreparer{to: "instinct"})
	hits := make(chan struct{}, 64)
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits <- struct{}{}
	}))
	t.Cleanup(hook.Close)
	const token = "tok-owner-notice-secret"
	w := wake.New(wake.Config{"grokbot": {Method: wake.Webhook, URL: hook.URL + "/hooks/grokbot", BearerToken: token, MaxPerHour: 100}}, h.st, wake.Options{
		HTTP:       hook.Client(),
		Debounce:   time.Millisecond,
		WakeGrace:  20 * time.Millisecond,
		Queued:     h.srv.QueuedCount,
		LastPoll:   h.srv.LastPoll,
		Unanswered: h.srv.TellAskers,
	})
	t.Cleanup(w.Stop)
	h.srv.SetWakeNamer(w)
	h.srv.SetEvents(w)

	h.send(museAddr, "grokbot", "are you there")
	var got []envelope.Request
	n := 0
	deadline := time.Now().Add(10 * time.Second)
	for n < 7 && time.Now().Before(deadline) {
		select {
		case <-hits:
			n++
		case <-time.After(20 * time.Millisecond):
		}
		got = append(got, ownerNotices(t, h, instinctAddr)...)
	}
	if n < 7 {
		t.Fatalf("webhook calls = %d, want at least 7", n)
	}
	got = append(got, ownerNotices(t, h, instinctAddr)...)
	if len(got) != 1 {
		t.Fatalf("owner notices = %+v, want exactly one", got)
	}
	for _, secret := range []string{hook.URL, "127.0.0.1", "/hooks/grokbot", token} {
		if strings.Contains(got[0].Body, secret) {
			t.Errorf("owner notice leaks %q: %s", secret, got[0].Body)
		}
	}
	if !strings.Contains(got[0].Body, "wake path: webhook") {
		t.Errorf("owner notice lacks the wake path: %s", got[0].Body)
	}
}
