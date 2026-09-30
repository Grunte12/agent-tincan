package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// dotOutRig is dot-web with its outbound watcher on: the fake dots
// extension, a test relay with dot-web and codex joined beside the mesh's
// agents (muse among them), and a
// state dir. clock drives the site cooldown.
type dotOutRig struct {
	t     *testing.T
	mesh  *testrelay.Mesh
	agent *WebAgent
	b     *dotsBrowser
	muse  *client.Relay
	dir   string
	clock *fakeClock
	seq   int
}

func newDotOutRig(t *testing.T) *dotOutRig {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	me := m.JoinOnMachineOf(t, "instinct", "dot-web")
	m.JoinOnMachineOf(t, "instinct", "codex")
	muse := m.Client(t, "muse")
	r := &dotOutRig{t: t, mesh: m, b: &dotsBrowser{}, muse: muse, dir: t.TempDir(), clock: newFakeClock()}
	r.agent = r.newAgent(me)
	return r
}

// newAgent is a fresh dot-web process over the rig's browser and state
// files (a restart).
func (r *dotOutRig) newAgent(me *client.Relay) *WebAgent {
	send := filepath.Join(r.dir, "dot-web-send.txt")
	return &WebAgent{
		Relay:             me,
		Site:              SourceDots,
		Name:              "dot-web",
		Native:            &Client{Channel: r.b, Cooldown: &SiteCooldown{Now: r.clock.Now}},
		Allowlist:         StaticAllowlist(AllowAll),
		Thread:            dotThread,
		JournalPath:       filepath.Join(r.dir, "dot-web-journal.json"),
		OutPath:           filepath.Join(r.dir, "dot-web-out.json"),
		SendAllowlist:     FileAllowlist(send),
		SendAllowlistPath: send,
		TempDir:           r.t.TempDir(),
		Log:               testLog{r.t},
		PollInterval:      5 * time.Millisecond,
		ClaudeStableFor:   time.Nanosecond,
	}
}

func (r *dotOutRig) restart() {
	r.agent = r.newAgent(r.agent.Relay)
}

// dotSays adds a message from the dot to the DM.
func (r *dotOutRig) dotSays(text string) string {
	return r.says(dotBot, text)
}

func (r *dotOutRig) says(from, text string) string {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	r.seq++
	id := "dot-msg-" + string(rune('a'+r.seq))
	r.b.items = append(r.b.items, dotItem(id, from, text, time.Now()))
	return id
}

func (r *dotOutRig) tick() time.Duration {
	r.t.Helper()
	return r.agent.dotTick(r.t.Context())
}

// typed is every message typed into the DM, in order.
func (r *dotOutRig) typed() []string {
	var out []string
	for _, a := range r.b.sent() {
		out = append(out, a.Message)
	}
	return out
}

// museInbox takes muse's waiting requests.
func (r *dotOutRig) museInbox() []envelope.Request {
	r.t.Helper()
	in, err := r.muse.PollReplies(r.t.Context(), 0, client.RepliesNone)
	if err != nil {
		r.t.Fatal(err)
	}
	return in.Requests
}

func (r *dotOutRig) museReplies(id, body string, status envelope.Status, attach ...string) {
	r.t.Helper()
	if _, err := r.muse.Claim(r.t.Context(), id); err != nil {
		r.t.Fatal(err)
	}
	var ids []string
	if len(attach) > 0 {
		ups, err := r.muse.UploadFiles(r.t.Context(), attach)
		if err != nil {
			r.t.Fatal(err)
		}
		ids = client.AttachmentIDs(ups)
	}
	if _, err := r.muse.ReplyAttached(r.t.Context(), id, body, status, ids); err != nil {
		r.t.Fatal(err)
	}
}

// taught runs the first tick, which teaches the dot, and checks it typed
// just the setup message.
func (r *dotOutRig) taught() {
	r.t.Helper()
	r.tick()
	if got := r.typed(); len(got) != 1 || !strings.Contains(got[0], "[tincan] You are connected") {
		r.t.Fatalf("first tick typed %q", got)
	}
}

func TestParseDotAsk(t *testing.T) {
	for _, c := range []struct {
		in                 string
		ok                 bool
		target, text, line string
		bad                string
	}{
		{in: "@tincan ask muse check the calendar", ok: true, target: "muse", text: "check the calendar", line: "@tincan ask muse check the calendar"},
		{in: "@Tincan ASK Muse: check\nthe calendar\n", ok: true, target: "muse", text: "check\nthe calendar", line: "@Tincan ASK Muse: check"},
		{in: "  @tincan ask codex\n\nfix the build  ", ok: true, target: "codex", text: "fix the build", line: "@tincan ask codex"},
		{in: "@tincan ask muse", ok: true, target: "muse", text: "", line: "@tincan ask muse"},
		{in: "@tincan ask", ok: true, bad: "no agent"},
		{in: "@tincan ask bad_name! hi", ok: true, bad: "not an agent name"},
		{in: "@tincan askmuse hi", ok: false},
		{in: "hello\n@tincan ask muse hi", ok: false},
		{in: "[tincan-reply from muse]\n> @tincan ask muse hi", ok: false},
		{in: "", ok: false},
	} {
		got, ok := parseDotAsk(c.in)
		if ok != c.ok {
			t.Fatalf("%q: ok = %v", c.in, ok)
		}
		if !ok {
			continue
		}
		if c.bad != "" {
			if !strings.Contains(got.bad, c.bad) {
				t.Fatalf("%q: bad = %q, want %q", c.in, got.bad, c.bad)
			}
			continue
		}
		if got.bad != "" || got.target != c.target || got.text != c.text || got.line != c.line {
			t.Fatalf("%q: %+v", c.in, got)
		}
	}
}

// The whole round trip: one ask to muse with the request text, never a
// second one (next poll, restart), and the answer typed back once.
func TestDotOutAskIsSentOnceAndItsAnswerTypedBack(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse check the calendar")
	if d := r.tick(); d != DefaultDotWatchInterval {
		t.Fatalf("next tick in %s", d)
	}
	reqs := r.museInbox()
	if len(reqs) != 1 || reqs[0].Body != "check the calendar" || reqs[0].From != "dot-web" || reqs[0].Kind != envelope.KindAsk {
		t.Fatalf("muse got %+v", reqs)
	}
	r.tick()
	r.restart()
	r.tick()
	if again := r.museInbox(); len(again) != 0 {
		t.Fatalf("asked again: %+v", again)
	}
	if got := r.typed(); len(got) != 1 {
		t.Fatalf("typed before the answer: %q", got)
	}
	info, err := os.Stat(r.agent.OutPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v %v", info, err)
	}

	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)
	r.tick()
	want := "[tincan-reply from muse]\n> @tincan ask muse check the calendar\n\nFriday is free."
	if got := r.typed(); len(got) != 2 || got[1] != want {
		t.Fatalf("typed %q\nwant %q", got, want)
	}
	r.tick()
	r.restart()
	r.tick()
	if got := r.typed(); len(got) != 2 {
		t.Fatalf("typed again: %q", got)
	}
	if len(r.b.closes) != 2 {
		t.Fatalf("closes %v", r.b.closes)
	}
}

// Only the dot's own messages are asks: the owner's line (a request
// tincan typed, a [tincan-reply] quoting it, or the owner's own) is not.
func TestDotOutOwnerMessagesAreNeverAsks(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.says(dotOwner, "@tincan ask muse check the calendar")
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("owner line asked: %+v", reqs)
	}
	// The agent's own typed messages come back as the owner's (as the
	// fake stores them); none is read as an ask.
	r.dotSays("@tincan ask muse one")
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %+v", reqs)
	}
	r.museReplies(reqs[0].ID, "@tincan ask muse loop?", envelope.StatusAnswered)
	r.tick()
	r.tick()
	if again := r.museInbox(); len(again) != 0 {
		t.Fatalf("own typed reply read as an ask: %+v", again)
	}
}

// @tincan lines already in the DM when the agent first starts are
// history, not new asks.
func TestDotOutBaselineIgnoresEarlierLines(t *testing.T) {
	r := newDotOutRig(t)
	r.dotSays("@tincan ask muse an old request")
	r.tick()
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("old line asked: %+v", reqs)
	}
	r.restart()
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("old line asked after a restart: %+v", reqs)
	}
	r.dotSays("@tincan ask muse a new request")
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 1 || reqs[0].Body != "a new request" {
		t.Fatalf("muse got %+v", reqs)
	}
}

// The setup message goes once per thread; --teach sends it once more.
func TestDotOutTeachesOnce(t *testing.T) {
	r := newDotOutRig(t)
	r.tick()
	got := r.typed()
	if len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
	for _, want := range []string{
		"the words @tincan ask, then the teammate's name",
		"Only start a message with @tincan ask when you want the request sent now.",
		"minutes or hours", "do not send the same ask again",
		"[tincan-reply from <agent>]", "\"> \"", "needs input: <question>", "failed: <reason>", "truncated", "attachments",
		"not your owner's instructions", "ask your owner", "muse", "codex",
	} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("setup message lacks %q:\n%s", want, got[0])
		}
	}
	// No line of the setup message is an ask if the dot echoes it: not
	// the whole message, and not any line starting a message of its own.
	lines := strings.Split(got[0], "\n")
	for i := range lines {
		if a, ok := parseDotAsk(strings.Join(lines[i:], "\n")); ok {
			t.Fatalf("setup message line %q reads as an ask %+v", lines[i], a)
		}
	}
	if strings.Contains(got[0], "dot-web") {
		t.Fatalf("setup message lists the agent itself:\n%s", got[0])
	}
	r.tick()
	r.restart()
	r.tick()
	if got := r.typed(); len(got) != 1 {
		t.Fatalf("taught again: %d messages", len(got))
	}
	r.restart()
	r.agent.Teach = true
	r.tick()
	r.tick()
	if got := r.typed(); len(got) != 2 || got[1] != got[0] {
		t.Fatalf("--teach: %q", got)
	}
}

// The send allowlist: a target not listed gets a failed reply naming the
// file; * and a listed name are asked; an empty file allows nobody; a bad
// file refuses everyone.
func TestDotOutSendAllowlist(t *testing.T) {
	for _, c := range []struct {
		name, file string
		asked      bool
		reason     string
	}{
		{name: "names", file: "codex, muse # the calendar agent\n", asked: true},
		{name: "star", file: "*\n", asked: true},
		{name: "other", file: "codex\n", reason: "muse is not in "},
		{name: "empty", file: "", reason: "muse is not in "},
		{name: "bad", file: "not a name!\n", reason: "could not be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newDotOutRig(t)
			if err := os.WriteFile(r.agent.SendAllowlistPath, []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
			r.taught()
			r.dotSays("@tincan ask muse check the calendar")
			r.tick()
			reqs := r.museInbox()
			if c.asked {
				if len(reqs) != 1 {
					t.Fatalf("muse got %+v", reqs)
				}
				return
			}
			if len(reqs) != 0 {
				t.Fatalf("muse asked: %+v", reqs)
			}
			got := r.typed()
			if len(got) != 2 || !strings.HasPrefix(got[1], "[tincan-reply from muse] failed: ") || !strings.Contains(got[1], c.reason) ||
				!strings.Contains(got[1], "dot-web-send.txt") || !strings.HasSuffix(got[1], "\n> @tincan ask muse check the calendar") {
				t.Fatalf("typed %q", got)
			}
			r.tick()
			if got := r.typed(); len(got) != 2 {
				t.Fatalf("failure typed twice: %q", got)
			}
		})
	}
}

// A declined or failed ask is typed back once with its reason; needs
// input asks the dot to ask again.
func TestDotOutUnansweredOutcomes(t *testing.T) {
	for _, c := range []struct {
		status envelope.Status
		body   string
		want   string
	}{
		{envelope.StatusDeclined, "not today", "[tincan-reply from muse] failed: declined: not today\n> @tincan ask muse check the calendar"},
		{envelope.StatusFailed, "the calendar is down", "[tincan-reply from muse] failed: failed: the calendar is down\n> @tincan ask muse check the calendar"},
		{envelope.StatusNeedsInput, "Which calendar?", "[tincan-reply from muse] needs input: Which calendar?\n> @tincan ask muse check the calendar\n\n"},
	} {
		t.Run(string(c.status), func(t *testing.T) {
			r := newDotOutRig(t)
			r.taught()
			r.dotSays("@tincan ask muse check the calendar")
			r.tick()
			reqs := r.museInbox()
			if len(reqs) != 1 {
				t.Fatalf("muse got %+v", reqs)
			}
			r.museReplies(reqs[0].ID, c.body, c.status)
			r.tick()
			r.tick()
			got := r.typed()
			if len(got) != 2 || !strings.HasPrefix(got[1], c.want) {
				t.Fatalf("typed %q\nwant prefix %q", got, c.want)
			}
		})
	}
}

// The reply text for each final status, expired included.
func TestDotReplyText(t *testing.T) {
	line := "@tincan ask muse hi"
	res := func(st envelope.Status, body string, files int) client.Result {
		r := client.Result{Status: st}
		if body != "" || files > 0 {
			r.Reply = &envelope.Reply{Body: body, Status: st}
			for range files {
				r.Reply.Attachments = append(r.Reply.Attachments, envelope.Attachment{})
			}
		}
		return r
	}
	for _, c := range []struct {
		r    client.Result
		want string
		done bool
	}{
		{res(envelope.StatusQueued, "", 0), "", false},
		{res(envelope.StatusHeld, "", 0), "", false},
		{res(envelope.StatusClaimed, "", 0), "", false},
		{res(envelope.StatusExpired, "", 0), "[tincan-reply from muse] failed: expired (no teammate answered in time)\n> @tincan ask muse hi", true},
		{res(envelope.StatusCancelled, "", 0), "[tincan-reply from muse] failed: cancelled\n> @tincan ask muse hi", true},
		{res(envelope.StatusAnswered, "", 0), "[tincan-reply from muse]\n> @tincan ask muse hi\n\n(the reply was empty)", true},
		{res(envelope.StatusAnswered, "two files", 2), "[tincan-reply from muse]\n> @tincan ask muse hi\n\ntwo files\n\n(2 attachments not shown)", true},
		{res(envelope.StatusAnswered, "one file", 1), "[tincan-reply from muse]\n> @tincan ask muse hi\n\none file\n\n(1 attachment not shown)", true},
	} {
		got, done := dotReplyText("muse", line, c.r)
		if got != c.want || done != c.done {
			t.Fatalf("%s: %q %v\nwant %q %v", c.r.Status, got, done, c.want, c.done)
		}
	}
	long, _ := dotReplyText("muse", line, res(envelope.StatusAnswered, strings.Repeat("x", 2*MaxSendMessage), 0))
	if len(long) > MaxSendMessage || !strings.Contains(long, "(reply truncated") {
		t.Fatalf("long reply %d bytes", len(long))
	}
}

// An answer's attachments are not carried into the DM; the reply says how
// many there were.
func TestDotOutAttachmentsAreNoted(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse send the chart")
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %+v", reqs)
	}
	img := filepath.Join(t.TempDir(), "chart.png")
	if err := os.WriteFile(img, fakePNG(500), 0o600); err != nil {
		t.Fatal(err)
	}
	r.museReplies(reqs[0].ID, "Here it is.", envelope.StatusAnswered, img)
	r.tick()
	if got := r.typed(); len(got) != 2 || !strings.HasSuffix(got[1], "Here it is.\n\n(1 attachment not shown)") {
		t.Fatalf("typed %q", got)
	}
}

// An @tincan line with no request text fails at once.
func TestDotOutEmptyRequestFails(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse")
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("muse got %+v", reqs)
	}
	if got := r.typed(); len(got) != 2 || got[1] != "[tincan-reply from muse] failed: empty request\n> @tincan ask muse" {
		t.Fatalf("typed %q", got)
	}
}

// A 429 on the feed read backs the watcher off for the site's
// Retry-After, holds every request to the site meanwhile, and the line
// written during it is asked once the read works again.
func TestDotOutRateLimitBacksOffAndKeepsTheLine(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse check the calendar")
	r.b.detailErr, r.b.retryAfter = "rate_limited", 120
	if d := r.tick(); d < 120*time.Second {
		t.Fatalf("next tick in %s after a 429 with retry_after 120", d)
	}
	if left := r.agent.Native.CooldownRemaining(SourceDots); left < 119*time.Second {
		t.Fatalf("cooldown %s", left)
	}
	// Still cooling down: the watcher does not read.
	r.b.detailErr = ""
	details := r.b.details
	if d := r.tick(); d < time.Minute || r.b.details != details {
		t.Fatalf("read during the cooldown (next %s)", d)
	}
	// Without Retry-After the backoff grows.
	_ = r.clock.Sleep(t.Context(), 3*time.Minute)
	r.b.detailErr, r.b.retryAfter = "rate_limited", 0
	d1 := r.tick()
	_ = r.clock.Sleep(t.Context(), d1)
	d2 := r.tick()
	if d1 < DefaultDotWatchInterval || d2 <= d1 {
		t.Fatalf("backoff %s then %s", d1, d2)
	}
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("muse got %+v", reqs)
	}
	_ = r.clock.Sleep(t.Context(), d2)
	r.b.detailErr = ""
	if d := r.tick(); d != DefaultDotWatchInterval {
		t.Fatalf("next tick in %s after a good read", d)
	}
	if reqs := r.museInbox(); len(reqs) != 1 || reqs[0].Body != "check the calendar" {
		t.Fatalf("muse got %+v", reqs)
	}
}

// Other read failures back off exponentially too.
func TestDotOutReadErrorsBackOff(t *testing.T) {
	r := newDotOutRig(t)
	r.b.detailErr = "endpoint_changed"
	d1 := r.tick()
	d2 := r.tick()
	if d1 <= DefaultDotWatchInterval || d2 <= d1 || len(r.typed()) != 0 {
		t.Fatalf("backoff %s then %s, typed %q", d1, d2, r.typed())
	}
}

// While an inbound request waits for the dot's answer, the watcher types
// nothing: an outbound reply typed then would be an owner message that
// orphans the inbound request.
func TestDotOutWaitsForAnInboundRequest(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %+v", reqs)
	}
	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)

	// codex asks the dot; the dot answers on the fourth read after the send.
	codex := r.mesh.Client(t, "codex")
	in, err := codex.Send(t.Context(), "dot-web", "what is on today?", envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	r.b.mu.Lock()
	r.b.replies = []dotReply{{text: "Nothing today.", at: 4}}
	r.b.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() {
		if n, err := r.agent.PollOnce(context.WithoutCancel(t.Context())); n != 1 || err != nil {
			t.Errorf("PollOnce = %d, %v", n, err)
		}
	})
	// Once the inbound message is typed, tick while it waits.
	deadline := time.Now().Add(10 * time.Second)
	for len(r.typed()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for waiting := true; waiting; {
		select {
		case <-done:
			waiting = false
		default:
			r.tick()
			time.Sleep(time.Millisecond)
		}
	}
	res, err := codex.Get(t.Context(), in.ID, 0)
	if err != nil || res.Status != envelope.StatusAnswered || !strings.HasPrefix(res.Reply.Body, "Nothing today.") {
		t.Fatalf("inbound %+v %v", res, err)
	}
	// A reply typed during the wait would have orphaned the inbound
	// request, so the answer above proves the watcher held off. A tick
	// that lands after the wait released the lock, before this loop saw
	// it end, may already have typed muse's reply; either way it is typed
	// after the inbound message, exactly once.
	if got := r.typed(); len(got) < 2 || got[1] != "what is on today?" {
		t.Fatalf("typed %q", got)
	}
	r.tick()
	if got := r.typed(); len(got) != 3 || !strings.HasPrefix(got[2], "[tincan-reply from muse]\n") {
		t.Fatalf("typed %q", got)
	}
}

// web serve runs the watcher beside the request loop: an outbound line
// round-trips to muse and back into the DM.
func TestDotOutRunsWithTheAgent(t *testing.T) {
	r := newDotOutRig(t)
	r.agent.WatchInterval = 5 * time.Millisecond
	r.agent.Hold = time.Second
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.agent.Run(ctx) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the setup message", func() bool { return len(r.typed()) == 1 })
	r.dotSays("@tincan ask muse check the calendar")
	var reqs []envelope.Request
	waitFor("the ask", func() bool { reqs = append(reqs, r.museInbox()...); return len(reqs) > 0 })
	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)
	waitFor("the answer in the DM", func() bool { return len(r.typed()) == 2 })
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatal(err)
	}
	if got := r.typed(); got[1] != "[tincan-reply from muse]\n> @tincan ask muse check the calendar\n\nFriday is free." {
		t.Fatalf("typed %q", got)
	}
}

// askFault is dot-web's relay transport with one fault on its next ask
// (POST /v1/send): "lost" delivers the ask and loses the response,
// "dropped" fails before the ask reaches the relay, "429" refuses it with
// a rate limit without delivering it, and "delivered" delivers it and then
// runs after. searchDown fails every search (GET /v1/search) with a 502.
type askFault struct {
	mu         sync.Mutex
	mode       string
	after      func()
	asks       int
	searches   int
	searchDown bool
}

func (f *askFault) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(client.AgentHeader, "dot-web")
	if req.Method == http.MethodGet && req.URL.Path == "/v1/search" {
		f.mu.Lock()
		f.searches++
		down := f.searchDown
		f.mu.Unlock()
		if down {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Request: req,
				Body: io.NopCloser(strings.NewReader(`{"error":"bad gateway"}`))}, nil
		}
		return http.DefaultTransport.RoundTrip(req)
	}
	if req.Method != http.MethodPost || req.URL.Path != "/v1/send" {
		return http.DefaultTransport.RoundTrip(req)
	}
	f.mu.Lock()
	mode := f.mode
	f.mode = ""
	f.asks++
	f.mu.Unlock()
	switch mode {
	case "lost":
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return nil, errors.New("connection reset by peer")
	case "dropped":
		return nil, errors.New("connection refused")
	case "429":
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Request: req,
			Body: io.NopCloser(strings.NewReader(`{"error":"slow down"}`))}, nil
	case "delivered":
		resp, err := http.DefaultTransport.RoundTrip(req)
		f.after()
		return resp, err
	}
	return http.DefaultTransport.RoundTrip(req)
}

// faultyRelay gives the rig's agent a relay client over an askFault.
func (r *dotOutRig) faultyRelay() *askFault {
	f := &askFault{}
	r.useFault(f)
	return f
}

func (r *dotOutRig) useFault(f *askFault) {
	r.agent.Relay = client.NewRelayHTTP(r.mesh.URL("dot-web"), &http.Client{Transport: f})
}

// edit changes the saved outbound state, as a process that died would
// have left it.
func (r *dotOutRig) edit(fn func(th *dotOutThread)) {
	r.t.Helper()
	st := r.agent.loadOut()
	fn(st.thread(dotThread))
	if err := r.agent.saveOut(st); err != nil {
		r.t.Fatal(err)
	}
}

// record is the thread's one record with status (nil when none).
func (r *dotOutRig) record(status string) *dotOutRecord {
	r.t.Helper()
	var found *dotOutRecord
	for _, rec := range r.agent.loadOut().thread(dotThread).Messages {
		if rec.Status == status {
			if found != nil {
				r.t.Fatalf("two %s records", status)
			}
			found = rec
		}
	}
	return found
}

// count is how many typed messages are text.
func (r *dotOutRig) count(text string) int {
	n := 0
	for _, m := range r.typed() {
		if m == text {
			n++
		}
	}
	return n
}

const calendarAnswer = "[tincan-reply from muse]\n> @tincan ask muse check the calendar\n\nFriday is free."

// The relay takes the ask but its response is lost: a later tick finds the
// ask on the relay and waits on it, so it is never sent again and its
// answer is typed back once.
func TestDotOutAskWithLostResponseIsAdopted(t *testing.T) {
	r := newDotOutRig(t)
	f := r.faultyRelay()
	r.taught()
	f.mode = "lost"
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	r.tick()
	r.restart()
	r.useFault(f)
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %d asks: %+v", len(reqs), reqs)
	}
	if rec := r.record(dotSent); rec == nil || rec.Request != reqs[0].ID || rec.Ask != "" {
		t.Fatalf("record %+v, want sent as %s", rec, reqs[0].ID)
	}
	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)
	r.tick()
	r.tick()
	if f.asks != 1 {
		t.Fatalf("%d asks sent", f.asks)
	}
	if got := r.typed(); len(got) != 2 || got[1] != calendarAnswer {
		t.Fatalf("typed %q", got)
	}
}

// The ask goes through but its record cannot be saved: the next tick finds
// the ask on the relay and does not ask again.
func TestDotOutAskIsNotResentWhenItsRecordIsNotSaved(t *testing.T) {
	r := newDotOutRig(t)
	state := filepath.Join(r.dir, "state")
	r.agent.OutPath = filepath.Join(state, "dot-web-out.json")
	f := r.faultyRelay()
	r.taught()
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })
	f.mode = "delivered"
	f.after = func() {
		if err := os.Chmod(state, 0o500); err != nil {
			t.Error(err)
		}
	}
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	r.tick()
	r.tick()
	if f.asks != 1 {
		t.Fatalf("%d asks sent", f.asks)
	}
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %d asks: %+v", len(reqs), reqs)
	}
	if rec := r.record(dotSent); rec == nil || rec.Request != reqs[0].ID {
		t.Fatalf("record %+v, want sent as %s", rec, reqs[0].ID)
	}
	if got := r.typed(); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
}

// An ask left sending that never reached the relay (the process died
// before asking, or the request failed before the relay got it) is asked
// once on a later tick, and the dot is not told delivery is uncertain.
func TestDotOutAskThatNeverReachedTheRelayIsAskedOnce(t *testing.T) {
	check := func(t *testing.T, r *dotOutRig, f *askFault, wantAsks int) {
		t.Helper()
		reqs := r.museInbox()
		if len(reqs) != 1 || reqs[0].Body != "check the calendar\nand the weekend" {
			t.Fatalf("muse got %+v", reqs)
		}
		r.tick()
		r.restart()
		r.useFault(f)
		r.tick()
		if again := r.museInbox(); len(again) != 0 {
			t.Fatalf("asked again: %+v", again)
		}
		if f.asks != wantAsks {
			t.Fatalf("%d asks sent", f.asks)
		}
		if rec := r.record(dotSent); rec == nil || rec.Request != reqs[0].ID || rec.Ask != "" {
			t.Fatalf("record %+v", rec)
		}
		if got := r.typed(); len(got) != 1 {
			t.Fatalf("typed %q", got)
		}
	}
	t.Run("died before asking", func(t *testing.T) {
		r := newDotOutRig(t)
		f := r.faultyRelay()
		r.taught()
		id := r.dotSays("@tincan ask muse check the calendar\nand the weekend")
		r.edit(func(th *dotOutThread) {
			th.Messages[id] = &dotOutRecord{Status: dotSending, Target: "muse", Line: "@tincan ask muse check the calendar",
				Ask: "check the calendar\nand the weekend", At: time.Now().UTC()}
		})
		r.restart()
		r.useFault(f)
		r.tick()
		check(t, r, f, 1)
	})
	t.Run("request failed before the relay", func(t *testing.T) {
		r := newDotOutRig(t)
		f := r.faultyRelay()
		r.taught()
		f.mode = "dropped"
		r.dotSays("@tincan ask muse check the calendar\nand the weekend")
		r.tick()
		if reqs := r.museInbox(); len(reqs) != 0 {
			t.Fatalf("muse got %+v", reqs)
		}
		if rec := r.record(dotSending); rec == nil {
			t.Fatalf("no sending record")
		}
		r.tick()
		check(t, r, f, 2)
	})
}

// While the relay cannot be searched, an ask left sending is neither asked
// again nor reported; once it has been unconfirmed for longer than
// dotOutReconcileFor the dot is told delivery is uncertain, once.
func TestDotOutAskIsKeptWhileTheRelayCannotBeSearched(t *testing.T) {
	r := newDotOutRig(t)
	f := r.faultyRelay()
	r.taught()
	f.mode = "dropped"
	f.searchDown = true
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	r.tick()
	r.restart()
	r.useFault(f)
	r.tick()
	if f.asks != 1 || f.searches == 0 {
		t.Fatalf("%d asks, %d searches", f.asks, f.searches)
	}
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("muse got %+v", reqs)
	}
	if got := r.typed(); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
	if rec := r.record(dotSending); rec == nil || rec.Ask != "check the calendar" {
		t.Fatalf("record %+v", rec)
	}
	r.edit(func(th *dotOutThread) {
		for _, rec := range th.Messages {
			rec.At = rec.At.Add(-2 * dotOutReconcileFor)
		}
	})
	r.tick()
	r.tick()
	want := "[tincan-reply from muse] failed: delivery uncertain (Tincan could not confirm the ask reached muse); ask again if you still need it\n> @tincan ask muse check the calendar"
	if got := r.typed(); len(got) != 2 || got[1] != want {
		t.Fatalf("typed %q\nwant %q", got, want)
	}
	if f.asks != 1 {
		t.Fatalf("%d asks sent", f.asks)
	}
}

// answeredAndLeftTyping asks muse, has it answer, and leaves the reply
// marked typing at at, as a process that died mid-typing would.
func answeredAndLeftTyping(t *testing.T, r *dotOutRig, at time.Time) {
	t.Helper()
	r.taught()
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %+v", reqs)
	}
	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)
	r.edit(func(th *dotOutThread) {
		for _, rec := range th.Messages {
			if rec.Status == dotSent {
				rec.Typing, rec.Intent = true, &dotIntent{Text: calendarAnswer, At: at}
			}
		}
	})
	r.restart()
}

// A reply whose typing was started but not confirmed is looked for in the
// DM: found, it is not typed again; not there while the feed reaches back
// past the typing, it is typed once; when the feed does not reach back that
// far it cannot be told, and it is not typed again.
func TestDotOutReplyLeftTyping(t *testing.T) {
	done := func(t *testing.T, r *dotOutRig) {
		t.Helper()
		for id, rec := range r.agent.loadOut().thread(dotThread).Messages {
			if rec.Status == dotOwn {
				continue
			}
			if !rec.Done || rec.Typing || rec.Intent != nil {
				t.Fatalf("%s left %+v", id, rec)
			}
		}
	}
	t.Run("in the DM", func(t *testing.T) {
		r := newDotOutRig(t)
		answeredAndLeftTyping(t, r, time.Now().UTC())
		id := r.says(dotOwner, calendarAnswer)
		r.tick()
		r.tick()
		if got := r.typed(); len(got) != 1 {
			t.Fatalf("retyped: %q", got)
		}
		done(t, r)
		if rec := r.agent.loadOut().thread(dotThread).Messages[id]; rec == nil || rec.Status != dotOwn {
			t.Fatalf("the typed reply is not recorded as own: %+v", rec)
		}
	})
	t.Run("not in the DM", func(t *testing.T) {
		r := newDotOutRig(t)
		answeredAndLeftTyping(t, r, time.Now().UTC())
		r.tick()
		r.restart()
		r.tick()
		if got := r.typed(); len(got) != 2 || !strings.HasSuffix(got[1], calendarAnswer) {
			t.Fatalf("typed %q", got)
		}
		done(t, r)
	})
	t.Run("not in a full DM that reaches back", func(t *testing.T) {
		r := newDotOutRig(t)
		answeredAndLeftTyping(t, r, time.Now().UTC().Add(10*time.Minute))
		r.flood(dotFeedWindow)
		r.tick()
		r.tick()
		n := 0
		for _, m := range r.typed() {
			if strings.HasSuffix(m, calendarAnswer) {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("typed the reply %d times: %q", n, r.typed())
		}
		done(t, r)
	})
	t.Run("feed does not reach back", func(t *testing.T) {
		r := newDotOutRig(t)
		answeredAndLeftTyping(t, r, time.Now().UTC().Add(-10*time.Minute))
		r.flood(dotFeedWindow)
		r.tick()
		r.tick()
		for _, m := range r.typed() {
			if strings.HasSuffix(m, calendarAnswer) {
				t.Fatalf("retyped: %q", r.typed())
			}
		}
		done(t, r)
	})
}

// The setup message left teaching is looked for in the DM like a reply:
// found, the dot counts as taught; not there, it is typed once.
func TestDotOutSetupLeftTeaching(t *testing.T) {
	leave := func(t *testing.T, r *dotOutRig) string {
		t.Helper()
		text := r.agent.dotSetupMessage(t.Context())
		r.edit(func(th *dotOutThread) {
			th.Started = time.Now().UTC()
			th.Teaching, th.TeachingIntent = true, &dotIntent{Text: text, At: time.Now().UTC()}
		})
		r.restart()
		return text
	}
	t.Run("in the DM", func(t *testing.T) {
		r := newDotOutRig(t)
		text := leave(t, r)
		r.says(dotOwner, text)
		r.tick()
		r.tick()
		if got := r.typed(); len(got) != 0 {
			t.Fatalf("retyped: %q", got)
		}
		if th := r.agent.loadOut().thread(dotThread); !th.Taught || th.Teaching || th.TeachingIntent != nil {
			t.Fatalf("thread %+v", th)
		}
	})
	t.Run("not in the DM", func(t *testing.T) {
		r := newDotOutRig(t)
		text := leave(t, r)
		r.tick()
		r.restart()
		r.tick()
		if got := r.typed(); len(got) != 1 || got[0] != text {
			t.Fatalf("typed %q", got)
		}
		if th := r.agent.loadOut().thread(dotThread); !th.Taught || th.Teaching || th.TeachingIntent != nil {
			t.Fatalf("thread %+v", th)
		}
	})
}

// Typing that fails before the send is clicked typed nothing: the reply
// stays due and is typed once the DM works again.
func TestDotOutUnclickedTypingFailureIsRetried(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	reqs := r.museInbox()
	if len(reqs) != 1 {
		t.Fatalf("muse got %+v", reqs)
	}
	r.museReplies(reqs[0].ID, "Friday is free.", envelope.StatusAnswered)
	r.b.mu.Lock()
	r.b.sendErr = "paused"
	r.b.mu.Unlock()
	r.tick()
	r.b.mu.Lock()
	r.b.sendErr = ""
	r.b.mu.Unlock()
	r.restart()
	r.tick()
	r.tick()
	// The fake records the failed attempt too: one unclicked, then one typed.
	if got := r.typed(); len(got) != 3 || got[1] != got[2] || !strings.HasSuffix(got[2], "Friday is free.") {
		t.Fatalf("typed %q", got)
	}
}

// A 429 from the relay means the ask was not taken: the line stays due
// and is asked once on a later tick.
func TestDotOutRelayRateLimitKeepsTheLine(t *testing.T) {
	r := newDotOutRig(t)
	f := r.faultyRelay()
	r.taught()
	f.mode = "429"
	r.dotSays("@tincan ask muse check the calendar")
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 0 {
		t.Fatalf("muse got %+v", reqs)
	}
	r.tick()
	r.tick()
	if reqs := r.museInbox(); len(reqs) != 1 || reqs[0].Body != "check the calendar" {
		t.Fatalf("muse got %+v", reqs)
	}
	if f.asks != 2 {
		t.Fatalf("%d asks sent", f.asks)
	}
	if got := r.typed(); len(got) != 1 {
		t.Fatalf("typed %q", got)
	}
}

// flood adds n messages from the dot dated a minute from now, then keeps
// only the latest dotFeedWindow messages, as the extension's dots.detail
// does.
func (r *dotOutRig) flood(n int) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	at := time.Now().Add(time.Minute)
	for range n {
		r.seq++
		r.b.items = append(r.b.items, dotItem(fmt.Sprintf("flood-%d", r.seq), dotBot, "chatter", at))
	}
	if len(r.b.items) > dotFeedWindow {
		r.b.items = r.b.items[len(r.b.items)-dotFeedWindow:]
	}
}

// gapNotes counts the notes typed about messages the window skipped.
func (r *dotOutRig) gapNotes() int {
	n := 0
	for _, m := range r.typed() {
		if m == dotGapNote {
			n++
		}
	}
	return n
}

// More messages than one read takes arrived between two reads: the dot is
// told once that an ask may have been missed, and not again.
func TestDotOutNotesMessagesPastTheWindowOnce(t *testing.T) {
	r := newDotOutRig(t)
	r.taught()
	r.tick() // sees the setup message
	r.flood(dotFeedWindow + 8)
	r.tick()
	if n := r.gapNotes(); n != 1 {
		t.Fatalf("%d gap notes, typed %q", n, r.typed())
	}
	r.tick()
	r.restart()
	r.tick()
	if n := r.gapNotes(); n != 1 {
		t.Fatalf("%d gap notes after more ticks", n)
	}
	if !strings.Contains(dotGapNote, "@tincan ask") || !strings.Contains(dotGapNote, "[tincan-reply]") {
		t.Fatalf("note: %q", dotGapNote)
	}
	if _, ok := parseDotAsk(dotGapNote); ok {
		t.Fatalf("the note reads as an ask")
	}
}

// No note when the window still holds the last message seen, when the
// feed is not full, or on the first read of a thread.
func TestDotOutNoGapNote(t *testing.T) {
	t.Run("last seen in the window", func(t *testing.T) {
		r := newDotOutRig(t)
		r.taught()
		r.tick()
		r.flood(dotFeedWindow - 1)
		r.tick()
		if n := r.gapNotes(); n != 0 {
			t.Fatalf("typed %q", r.typed())
		}
	})
	t.Run("feed not full", func(t *testing.T) {
		r := newDotOutRig(t)
		r.taught()
		r.tick()
		r.b.mu.Lock()
		r.b.items = nil
		r.b.mu.Unlock()
		r.flood(10)
		r.tick()
		if n := r.gapNotes(); n != 0 {
			t.Fatalf("typed %q", r.typed())
		}
	})
	t.Run("first read", func(t *testing.T) {
		r := newDotOutRig(t)
		r.flood(dotFeedWindow + 8)
		r.tick()
		r.tick()
		if n := r.gapNotes(); n != 0 {
			t.Fatalf("typed %q", r.typed())
		}
	})
}

// A full read whose oldest message shares the last-seen message's
// timestamp, without that message, still counts as a gap.
func TestDotOutGapNoteOnTiedTimestamp(t *testing.T) {
	at := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	th := &dotOutThread{LastSeenID: "seen", LastSeenAt: at}
	var feed DotFeed
	for i := range dotFeedWindow {
		feed.Messages = append(feed.Messages, DotMessage{ID: fmt.Sprintf("m%02d", i), At: at, Text: "hi"})
	}
	th.noteGap(feed)
	if !th.GapNote {
		t.Fatal("no gap noted for a full read tied with the last-seen timestamp")
	}
}
