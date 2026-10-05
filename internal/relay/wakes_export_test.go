package relay

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

// wakeExportHarness wires a real waker into the relay: grokbot on a webhook
// that answers with status, instinct on email through the same server.
func wakeExportHarness(t *testing.T, status int) (*harness, *wake.Waker) {
	t.Helper()
	h := newHarness(t, Config{})
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(hook.Close)
	w := wake.New(wake.Config{
		"grokbot":  {Method: wake.Webhook, URL: hook.URL + "/hooks/URLPATHSECRET?token=URLQUERYSECRET", BearerToken: "BEARERSECRET"},
		"instinct": {Method: wake.Email, EmailTo: "owner@example.com", AgentMailFrom: "bot@agentmail.to", AgentMailKey: "EMAILKEYSECRET"},
	}, h.st, wake.Options{
		HTTP: hook.Client(), AgentMailAPI: hook.URL, Debounce: time.Millisecond, RetryDelay: time.Millisecond,
		WakeGrace: -1, ReplyRetries: []time.Duration{}, Queued: h.srv.QueuedCount, LastPoll: h.srv.LastPoll,
	})
	t.Cleanup(w.Stop)
	h.srv.SetWakeNamer(w)
	h.srv.SetEvents(w)
	return h, w
}

// wakeOnce queues an ask for agent and waits for the wake it sets off.
func wakeOnce(h *harness, w *wake.Waker, to string) {
	h.send(museAddr, to, "check the garage")
	w.Flush()
}

func exportWakes(t *testing.T, h *harness, addr, agent string, since time.Time, want int) WakeExport {
	t.Helper()
	var out WakeExport
	q := url.Values{"since": {since.Format(time.RFC3339Nano)}}
	h.do(addr, "GET", "/v1/admin/wakes/"+agent+"?"+q.Encode(), "", want, &out)
	return out
}

func auditCount(t *testing.T, st *store.Store, event, actor string) int {
	t.Helper()
	evs, err := st.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Event == event && e.Actor == actor {
			n++
		}
	}
	return n
}

// Wakes with no poll before the next one show no poll; the wake the agent
// finally polled after shows that poll, and its status and path.
func TestWakeExportPairsFirstPollAfterEachWake(t *testing.T) {
	h, w := wakeExportHarness(t, http.StatusOK)
	since := time.Now().Add(-time.Second)
	for range 3 {
		wakeOnce(h, w, "grokbot")
	}
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)

	out := exportWakes(t, h, macAddr, "grokbot", since, http.StatusOK)
	if out.Agent != "grokbot" || len(out.Wakes) != 3 {
		t.Fatalf("export = %+v, want 3 wakes", out)
	}
	for i, e := range out.Wakes {
		if e.Event != "woke" || e.Path != "webhook" || e.Status != "200" || e.Reply != ReplyNotRecorded || e.Error != "" {
			t.Errorf("wake %d = %+v", i, e)
		}
		if i < 2 && e.NextPoll != nil {
			t.Errorf("wake %d next poll = %v, want none: a later wake came first", i, e.NextPoll)
		}
	}
	last := out.Wakes[2]
	if last.NextPoll == nil || last.NextPoll.Before(last.At) || last.NextVia != "poll" {
		t.Fatalf("last wake = %+v, want the poll after it", last)
	}
}

// Only the first poll after a wake writes a polled audit row.
func TestPolledRowOncePerWake(t *testing.T) {
	h, w := wakeExportHarness(t, http.StatusOK)
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil) // no wake yet
	if n := auditCount(t, h.st, "polled", "grokbot"); n != 0 {
		t.Fatalf("polled rows before any wake = %d", n)
	}
	wakeOnce(h, w, "grokbot")
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusNoContent, nil)
	h.do(grokAddr, "GET", "/v1/poll?hold=0&peek=1", "", http.StatusNoContent, nil)
	if n := auditCount(t, h.st, "polled", "grokbot"); n != 1 {
		t.Fatalf("polled rows after three polls = %d, want 1", n)
	}
	time.Sleep(2 * time.Millisecond) // the next wake starts after the last poll
	wakeOnce(h, w, "grokbot")
	h.do(grokAddr, "GET", "/v1/poll?hold=0", "", http.StatusOK, nil)
	if n := auditCount(t, h.st, "polled", "grokbot"); n != 2 {
		t.Fatalf("polled rows after the second wake = %d, want 2", n)
	}
}

// A woke row from before the status code and polled rows existed reports
// the next claim as its next activity and its reply as not recorded.
func TestWakeExportOlderRowUsesNextClaim(t *testing.T) {
	h := newHarness(t, Config{})
	since := time.Now().Add(-time.Second)
	if err := h.st.Audit(t.Context(), store.AuditEvent{Event: "woke", Actor: "grokbot", Detail: "webhook, 1 waiting"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	req := h.send(museAddr, "grokbot", "check the garage")
	h.do(grokAddr, "POST", "/v1/requests/"+req.ID+"/claim", "", http.StatusOK, nil)

	out := exportWakes(t, h, macAddr, "grokbot", since, http.StatusOK)
	if len(out.Wakes) != 1 {
		t.Fatalf("export = %+v", out)
	}
	e := out.Wakes[0]
	if e.Path != "webhook" || e.Status != "2xx" || e.Reply != ReplyNotRecorded || e.NextPoll == nil || e.NextVia != "claimed" {
		t.Fatalf("older wake = %+v, want the claim as next activity", e)
	}
}

// A failed wake shows its reason and status, and no reply summary.
func TestWakeExportFailedWake(t *testing.T) {
	h, w := wakeExportHarness(t, http.StatusInternalServerError)
	since := time.Now().Add(-time.Second)
	wakeOnce(h, w, "grokbot")
	out := exportWakes(t, h, macAddr, "grokbot", since, http.StatusOK)
	if len(out.Wakes) != 1 {
		t.Fatalf("export = %+v", out)
	}
	e := out.Wakes[0]
	if e.Event != "wake_failed" || e.Path != "webhook" || e.Status != "500" || e.Reply != "" || !strings.Contains(e.Error, "500") || e.NextPoll != nil {
		t.Fatalf("failed wake = %+v", e)
	}
}

// A reply summary in the woke detail (as a relay that records one writes it)
// is shown as the reply.
func TestWakeExportShowsRecordedReply(t *testing.T) {
	h := newHarness(t, Config{})
	since := time.Now().Add(-time.Second)
	if err := h.st.Audit(t.Context(), store.AuditEvent{Event: "woke", Actor: "grokbot", Detail: "webhook, HTTP 200, 1 waiting, response: queued, run 42"}); err != nil {
		t.Fatal(err)
	}
	out := exportWakes(t, h, macAddr, "grokbot", since, http.StatusOK)
	if len(out.Wakes) != 1 || out.Wakes[0].Reply != "queued, run 42" || out.Wakes[0].Status != "200" {
		t.Fatalf("export = %+v", out)
	}
}

func TestWakeExportAdminOnly(t *testing.T) {
	h, _ := wakeExportHarness(t, http.StatusOK)
	exportWakes(t, h, grokAddr, "grokbot", time.Now().Add(-time.Hour), http.StatusForbidden)
	exportWakes(t, h, strangerAddr, "grokbot", time.Now().Add(-time.Hour), http.StatusForbidden)
}

func TestWakeExportEmptyWindow(t *testing.T) {
	h, w := wakeExportHarness(t, http.StatusOK)
	wakeOnce(h, w, "grokbot")
	var out WakeExport
	since := time.Now().Add(-time.Hour)
	q := url.Values{"since": {since.Format(time.RFC3339Nano)}, "until": {since.Add(time.Minute).Format(time.RFC3339Nano)}}
	rec := h.do(macAddr, "GET", "/v1/admin/wakes/grokbot?"+q.Encode(), "", http.StatusOK, &out)
	if out.Wakes == nil || len(out.Wakes) != 0 || !strings.Contains(rec.Body.String(), `"wakes":[]`) {
		t.Fatalf("empty window = %s", rec.Body.String())
	}
	h.do(macAddr, "GET", "/v1/admin/wakes/grokbot", "", http.StatusBadRequest, nil)
	h.do(macAddr, "GET", "/v1/admin/wakes/grokbot?since=yesterday", "", http.StatusBadRequest, nil)
}

// The export carries no webhook URL, bearer token or email key, for webhook
// and email wakes, sent or failed.
func TestWakeExportCarriesNoSecrets(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		h, w := wakeExportHarness(t, status)
		since := time.Now().Add(-time.Second)
		wakeOnce(h, w, "grokbot")
		wakeOnce(h, w, "instinct")
		for _, agent := range []string{"grokbot", "instinct"} {
			q := url.Values{"since": {since.Format(time.RFC3339Nano)}}
			rec := h.do(macAddr, "GET", "/v1/admin/wakes/"+agent+"?"+q.Encode(), "", http.StatusOK, nil)
			body := rec.Body.String()
			if !strings.Contains(body, `"event":"w`) {
				t.Fatalf("%d %s: no wake exported: %s", status, agent, body)
			}
			for _, secret := range []string{"SECRET", "http://", "https://", "/hooks", "token="} {
				if strings.Contains(body, secret) {
					t.Errorf("%d %s export leaks %q: %s", status, agent, secret, body)
				}
			}
		}
	}
}
