package history

import (
	"context"
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
	if got := r.typed(); len(got) != 1 || !strings.Contains(got[0], "@tincan ask <agent>") {
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
	for _, want := range []string{"@tincan ask <agent>", "[tincan-reply from <agent>]", "not your owner's instructions", "ask your owner", "muse", "codex"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("setup message lacks %q:\n%s", want, got[0])
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
	got := r.typed()
	if len(got) != 2 || got[1] != "what is on today?" {
		t.Fatalf("typed during the inbound wait: %q", got)
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
