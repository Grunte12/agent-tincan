package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	copilotConv1 = "c0b1107a-0000-4000-8000-000000000001"
	copilotConv2 = "c0b1107a-0000-4000-8000-000000000002"
	copilotConv3 = "c0b1107a-0000-4000-8000-000000000003"
)

// copilotMsg builds one message of the extension's copilot.detail result.
func copilotMsg(id, author, text string, at time.Time, sources ...copilotSource) map[string]any {
	if sources == nil {
		sources = []copilotSource{}
	}
	return map[string]any{"id": id, "author": author, "text": text, "createdAt": at.UTC().Format(time.RFC3339Nano), "sources": sources}
}

func copilotDetailJSON(id, title string, updated time.Time, msgs ...map[string]any) json.RawMessage {
	if msgs == nil {
		msgs = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"conversationId": id, "title": title, "createdAt": "", "updatedAt": updated.UTC().Format("2006-01-02T15:04:05.000"), "messages": msgs})
	return b
}

// copilotHistoryFake answers copilot.list with the sidebar's order (three
// chats, newest first, plus a malformed link the extension would not
// send but the reader must drop) and copilot.detail from the recorded
// fixture (conversation 1) or built conversations 2 and 3, 3 older than
// the default 30-day window.
func copilotHistoryFake(t *testing.T) *fakeChannel {
	return &fakeChannel{handle: func(req NativeRequest) ([]NativeResponse, error) {
		if err := ValidateOp(req.Op, req.Args); err != nil {
			return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: err.Error()}}}, nil
		}
		switch req.Op {
		case OpCopilotList:
			return []NativeResponse{{OK: true, Result: json.RawMessage(`{"conversations":[
				{"id":"` + strings.ToUpper(copilotConv1) + `","title":"Tin can telephones"},
				{"id":"not-a-uuid","title":"Broken"},
				{"id":"` + copilotConv2 + `","title":"Morse code basics"},
				{"id":"` + copilotConv3 + `","title":"Semaphore flags"}]}`)}}, nil
		case OpCopilotDetail:
			switch req.Args.ID {
			case copilotConv1:
				return []NativeResponse{{OK: true, Result: fixture(t, "copilot/detail-"+copilotConv1+".json")}}, nil
			case copilotConv2:
				at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
				return []NativeResponse{{OK: true, Result: copilotDetailJSON(copilotConv2, "Morse code basics", at.Add(time.Minute),
					copilotMsg("u2", "user", "What is SOS in morse?", at),
					copilotMsg("b2", "bot", "Three dots, three dashes, three dots.", at.Add(time.Minute)))}}, nil
			case copilotConv3:
				at := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
				return []NativeResponse{{OK: true, Result: copilotDetailJSON(copilotConv3, "Semaphore flags", at,
					copilotMsg("u3", "user", "How do semaphore flags work?", at))}}, nil
			}
			return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "404"}}}, nil
		}
		return nil, errors.New("unexpected op")
	}}
}

func newTestCopilot(ch Channel) *Copilot {
	r := NewCopilot(&Client{Channel: ch, Timeout: 2 * time.Second, Cooldown: &SiteCooldown{}})
	r.Now = func() time.Time { return liveNow }
	r.AgentChats = ""
	return r
}

func opCount(f *fakeChannel, op Op) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Op == op {
			n++
		}
	}
	return n
}

// The sidebar's list keeps its order, with canonical ids and no times;
// nothing in it is aged out, since it has no dates.
func TestCopilotList(t *testing.T) {
	r := newTestCopilot(copilotHistoryFake(t))
	convs, err := convsOf(r.List(context.Background(), 10, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range convs {
		got = append(got, c.ID+" "+c.Title)
		if !c.UpdatedAt.IsZero() || c.Source != SourceCopilot {
			t.Fatalf("conversation %+v", c)
		}
	}
	want := []string{copilotConv1 + " Tin can telephones", copilotConv2 + " Morse code basics", copilotConv3 + " Semaphore flags"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list\n%s", strings.Join(got, "\n"))
	}
}

// A conversation reads into turns from the recorded page JSON (cut down by
// the extension): tool context, search queries and suggestions are not
// turns, and each reply keeps its sources.
func TestCopilotReadConversation(t *testing.T) {
	th, err := parseCopilotDetail(copilotConv1, fixture(t, "copilot/detail-"+copilotConv1+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if th.conv.Title != "Tin can telephones" || !th.conv.UpdatedAt.Equal(time.Date(2026, 9, 20, 10, 5, 30, 0, time.UTC)) || len(th.turns) != 2 {
		t.Fatalf("thread %+v", th)
	}
	t0, t1 := th.turns[0], th.turns[1]
	if t0.prompt.Text != "How far can a tin can telephone carry a voice?" || t0.promptID != "m0000000-0000-4000-8000-000000000001" || !t0.prompt.Time.Equal(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("turn 0 prompt %+v", t0)
	}
	if t0.reply.Text != "About **30 metres** with a taut string [1][2]." || len(t0.replySources) != 2 || t0.replySources[1] != (webSource{title: "String Lab", url: "https://strings.example/range"}) {
		t.Fatalf("turn 0 reply %+v", t0)
	}
	if t1.reply.Text != "Wire carries it further, a few hundred metres." || len(t1.replySources) != 0 {
		t.Fatalf("turn 1 %+v", t1)
	}

	r := newTestCopilot(copilotHistoryFake(t))
	for _, id := range []string{copilotConv1, strings.ToUpper(copilotConv1)} {
		convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeConversation, ConversationID: id}, Options{}))
		if err != nil || len(convs) != 1 || len(convs[0].Messages) != 4 || convs[0].ID != copilotConv1 {
			t.Fatalf("%s: %+v %v", id, convs, err)
		}
	}
	if _, err := r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeConversation, ConversationID: "0e1d0000-0000-4000-8000"}, Options{}); err == nil {
		t.Fatal("a malformed id was read")
	}
	if _, err := parseCopilotDetail(copilotConv1, json.RawMessage(`{"conversationId":"x"}`)); err == nil {
		t.Fatal("a detail with no messages parsed")
	}
	// An odd timestamp is no time, never a failed read.
	th, err = parseCopilotDetail(copilotConv1, json.RawMessage(`{"messages":[{"id":"u","author":"user","text":"hi","createdAt":"yesterday"}],"updatedAt":17}`))
	if err != nil || len(th.turns) != 1 || !th.turns[0].prompt.Time.IsZero() {
		t.Fatalf("%+v %v", th, err)
	}
}

// With an undated list, each conversation read bounds the time of the
// ones after it (the sidebar is newest first), so the latest prompt needs
// only the reads that could still hold a newer one, not the whole window.
func TestCopilotLatestReadsOnlyWhatItNeeds(t *testing.T) {
	f := copilotHistoryFake(t)
	r := newTestCopilot(f)
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeLatest}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || convs[0].ID != copilotConv1 || convs[0].Messages[0].Text != "And with wire instead of string?" {
		t.Fatalf("latest %+v", convs)
	}
	if n := opCount(f, OpCopilotDetail); n != 2 {
		t.Fatalf("read %d conversations for the latest prompt, want 2 (the second could be as new as the first)", n)
	}
	// Search walks on, and stops at the window's age: conversation 3 is
	// older than 30 days, so it is never shown.
	p, err := r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeSearch, Terms: []string{"semaphore"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Conversations) != 0 {
		t.Fatalf("search %+v", p)
	}
	p, err = r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeSearch, Terms: []string{"morse"}}, Options{})
	if err != nil || len(p.Conversations) != 1 || p.Conversations[0].ID != copilotConv2 {
		t.Fatalf("search morse %+v %v", p, err)
	}
}

// Chats the copilot-web agent sent into are left out of history unless
// all is asked for, and are marked automated then.
func TestCopilotHistorySkipsWebAgentChats(t *testing.T) {
	r := newTestCopilot(copilotHistoryFake(t))
	r.AgentChats = filepath.Join(t.TempDir(), "web-agent-copilot-conversations.json")
	if err := recordWebUsed(r.AgentChats, copilotConv1, liveNow); err != nil {
		t.Fatal(err)
	}
	convs, err := convsOf(r.List(context.Background(), 10, Options{}))
	if err != nil || len(convs) != 2 || convs[0].ID != copilotConv2 {
		t.Fatalf("list without all %+v %v", convs, err)
	}
	latest, err := convsOf(r.Read(context.Background(), Query{Source: SourceCopilot, Mode: ModeLatest}, Options{}))
	if err != nil || len(latest) != 1 || latest[0].ID != copilotConv2 {
		t.Fatalf("latest without all %+v %v", latest, err)
	}
	all, err := convsOf(r.List(context.Background(), 10, Options{All: true}))
	if err != nil || len(all) != 3 || !all[0].Automated || all[1].Automated {
		t.Fatalf("list with all %+v %v", all, err)
	}
	if DefaultWebUsedPath(SourceCopilot) == "" || !strings.HasSuffix(NewCopilot(&Client{}).AgentChats, "web-agent-copilot-conversations.json") {
		t.Fatalf("used path %q", NewCopilot(&Client{}).AgentChats)
	}
}

// The sidebar read gets the longer tab-read timeout; the JSON read does
// not.
func TestCopilotListGetsTheTabReadTimeout(t *testing.T) {
	c := &Client{}
	if got := c.timeout(OpCopilotList); got != TabReadClientTimeout {
		t.Fatalf("list timeout %s", got)
	}
	if got := c.timeout(OpCopilotDetail); got != 30*time.Second {
		t.Fatalf("detail timeout %s", got)
	}
	if got := c.timeout(OpGrokList); got != 30*time.Second {
		t.Fatalf("grok list timeout %s", got)
	}
}

// Copilot gives no source numbers: the shared footer lists plain
// "- <title> <url>" lines, drops non-http(s), credentialed and repeated
// URLs, and counts what is over maxReplySources.
func TestSourcesFooterUnnumbered(t *testing.T) {
	if got := sourcesFooter(nil); got != "" {
		t.Fatalf("no sources: %q", got)
	}
	var many []webSource
	for i := range 13 {
		many = append(many, webSource{title: fmt.Sprintf("Source  %d\nline", i), url: fmt.Sprintf("https://s%d.example/p", i)})
	}
	many = append([]webSource{
		{title: "", url: "https://first.example/a"},
		{title: "dup", url: "https://first.example/a"},
		{title: "script", url: "javascript:alert(1)"},
		{title: "creds", url: "https://user:pw@evil.example/"},
		{title: "relative", url: "/chat/pages"},
	}, many...)
	got := sourcesFooter(many)
	lines := strings.Split(got, "\n")
	if lines[0] != "Sources:" || lines[1] != "- https://first.example/a" || lines[2] != "- Source 0 line https://s0.example/p" {
		t.Fatalf("footer:\n%s", got)
	}
	if len(lines) != 1+maxReplySources+1 || lines[len(lines)-1] != "(and 4 more)" {
		t.Fatalf("footer has %d lines:\n%s", len(lines), got)
	}
	for _, bad := range []string{"javascript", "evil.example", "/chat/pages", "dup"} {
		if strings.Contains(got, bad) {
			t.Fatalf("footer carries %q:\n%s", bad, got)
		}
	}
}

// ---- copilot-web.

// copilotBrowser plays the extension for copilot-web: copilot.send opens
// (or continues) a conversation, and copilot.detail shows each sent turn
// as the page JSON would: nothing for lag polls, then the answer growing
// for grow polls, then the whole answer with its sources.
type copilotBrowser struct {
	mu      sync.Mutex
	ops     []Op
	sends   []OpArgs
	closes  []string
	convs   map[string][]*copilotTurn
	seq     int
	lag     int
	grow    int
	sources []copilotSource
	// sendErr fails every send with that code and message.
	sendErr, sendMsg string
}

type copilotTurn struct {
	prompt, answer string
	userID, botID  string
	at             time.Time
	lag, grow      int
}

func (c *copilotBrowser) sent() []OpArgs {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]OpArgs(nil), c.sends...)
}

func (c *copilotBrowser) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, req.Op)
	fail := func(code, msg string) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: msg}})
		return err
	}
	if err := ValidateOp(req.Op, req.Args); err != nil {
		return fail("bad_request", err.Error())
	}
	switch req.Op {
	case OpCopilotSend:
		c.sends = append(c.sends, req.Args)
		if c.sendErr != "" {
			return fail(c.sendErr, c.sendMsg)
		}
		id := req.Args.ConversationID
		if id == "" {
			c.seq++
			id = fmt.Sprintf("c0b1107a-0000-4000-8000-%012d", 100+c.seq)
		} else if _, ok := c.convs[id]; !ok {
			return fail("not_found", "conversation not found")
		}
		n := len(c.convs[id])
		now := time.Now()
		c.convs[id] = append(c.convs[id], &copilotTurn{
			prompt: req.Args.Message, answer: "Copilot says: " + req.Args.Message,
			userID: fmt.Sprintf("u-%s-%d", id[len(id)-3:], n), botID: fmt.Sprintf("b-%s-%d", id[len(id)-3:], n),
			at: now, lag: c.lag, grow: c.grow,
		})
		res, _ := json.Marshal(SendResult{ConversationID: id, URL: "https://copilot.com/chat/conversation/" + id, SubmittedAt: now.UnixMilli()})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
		return err
	case OpCopilotDetail:
		turns, ok := c.convs[req.Args.ID]
		if !ok {
			return fail("not_found", "404")
		}
		var msgs []map[string]any
		for _, t := range turns {
			if t.lag > 0 {
				t.lag--
				continue
			}
			msgs = append(msgs, copilotMsg(t.userID, "user", t.prompt, t.at.Add(200*time.Millisecond)))
			if t.grow > 0 {
				msgs = append(msgs, copilotMsg(t.botID, "bot", t.answer[:len(t.answer)-t.grow], t.at.Add(time.Second)))
				t.grow--
				continue
			}
			msgs = append(msgs, copilotMsg(t.botID, "bot", t.answer, t.at.Add(time.Second), c.sources...))
		}
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: copilotDetailJSON(req.Args.ID, "chat", time.Now(), msgs...)})
		return err
	case OpCopilotClose:
		c.closes = append(c.closes, req.Args.ConversationID)
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
		return err
	}
	return fail("bad_request", "unexpected op")
}

func copilotRig(t *testing.T) (*webRig, *copilotBrowser) {
	t.Helper()
	rig := newWebRig(t)
	b := &copilotBrowser{convs: map[string][]*copilotTurn{}}
	rig.agent.Site = SourceCopilot
	rig.agent.Name = "copilot-web"
	rig.agent.Native = &Client{Channel: b, Cooldown: &SiteCooldown{}}
	rig.agent.ClaudeStableFor = time.Nanosecond
	return rig, b
}

// A send polls until the answer holds still (Copilot marks none
// finished), then replies with the text, a Sources footer and the Copilot
// conversation footer, closes the tab and records the chat for history to
// skip.
func TestCopilotWebReplyWithSources(t *testing.T) {
	rig, b := copilotRig(t)
	b.lag, b.grow = 1, 3
	b.sources = []copilotSource{{Title: "Example Physics", URL: "https://physics.example/tin-can"}, {Title: "again", URL: "https://physics.example/tin-can"}, {Title: "String Lab", URL: "https://strings.example/range"}}
	res := rig.ask(t, "codex", "How far does a tin can phone carry?")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	id := "c0b1107a-0000-4000-8000-000000000101"
	want := "Copilot says: How far does a tin can phone carry?\n\nSources:\n- Example Physics https://physics.example/tin-can\n- String Lab https://strings.example/range\n\nCopilot conversation: " + id
	if res.Reply.Body != want {
		t.Fatalf("reply\n%s\nwant\n%s", res.Reply.Body, want)
	}
	if len(b.closes) != 1 || b.closes[0] != id {
		t.Fatalf("closes %v", b.closes)
	}
	if used := loadWebUsed(rig.agent.UsedPath); !used[id] {
		t.Fatalf("conversation not recorded for history to skip: %v", used)
	}
	// Threading: the asker's next message continues the conversation; a
	// conversation: URL on copilot.com picks one.
	rig.ask(t, "codex", "and with wire?")
	res = rig.ask(t, "codex", "conversation: https://copilot.com/chat/conversation/"+strings.ToUpper(id)+"\npeek")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Copilot says: peek") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	got := b.sent()
	if len(got) != 3 || got[1] != (OpArgs{Message: "and with wire?", ConversationID: id}) || got[2] != (OpArgs{Message: "peek", ConversationID: id}) {
		t.Fatalf("sends %+v", got)
	}
	if res := rig.ask(t, "codex", "conversation: https://copilot.com/chat/conversation/not-a-uuid\nx"); res.Status != envelope.StatusFailed {
		t.Fatalf("a malformed Copilot URL: %s %q", res.Status, res.Reply.Body)
	}
}

// Sign-in, terms and work-account landings, the human check and a missing
// grant each say what the owner has to do; nothing is left to close, and
// only the human check holds Copilot back.
func TestCopilotWebSendFailureReplies(t *testing.T) {
	for _, c := range []struct{ code, msg, want string }{
		{"not_logged_in", "the page went to login.live.com", "open https://copilot.microsoft.com in Chrome, sign in with a personal Microsoft account and finish any Microsoft sign-in or terms prompt"},
		{"not_logged_in", "the page left copilot.com for a sign-in or terms page", "finish any Microsoft sign-in or terms prompt"},
		{"not_logged_in", "work or school account: the page went to m365.cloud.microsoft", "work or school account; copilot-web needs a personal Microsoft account"},
		{"blocked", "anti-bot check on copilot.com", "copilot.com showed an anti-bot check; open copilot.com in Chrome, complete the check"},
		{"permission_missing", "no access", "has no access to copilot.com; grant it on the extension's options page"},
	} {
		rig, b := copilotRig(t)
		cd := &SiteCooldown{}
		rig.agent.Native.Cooldown = cd
		b.sendErr, b.sendMsg = c.code, c.msg
		res := rig.ask(t, "codex", "hello")
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, c.want) {
			t.Errorf("%s %q: %s %q", c.code, c.msg, res.Status, res.Reply.Body)
		}
		if len(b.closes) != 0 {
			t.Errorf("%s: closes %v", c.code, b.closes)
		}
		blocked := c.code == "blocked"
		if left := cd.Remaining(SourceCopilot); (left > 0) != blocked || cd.Remaining(SourceChatGPT) != 0 {
			t.Errorf("%s: copilot cooldown %s, chatgpt %s", c.code, left, cd.Remaining(SourceChatGPT))
		}
		if blocked {
			res := rig.ask(t, "codex", "again")
			if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "anti-bot check") || len(b.sent()) != 1 {
				t.Errorf("during the cooldown: %s %q sends %d", res.Status, res.Reply.Body, len(b.sent()))
			}
		}
	}
}

// Copilot's reply wait holds out for source links that land after the
// text: the same text with more sources is not the same read.
func TestCopilotReplySignatureCountsSources(t *testing.T) {
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	sig := func(srcs ...copilotSource) string {
		raw := copilotDetailJSON(copilotConv1, "t", at, copilotMsg("u1", "user", "q", at), copilotMsg("b1", "bot", "answer", at.Add(time.Second), srcs...))
		nodes, err := copilotNodes(raw)
		if err != nil {
			t.Fatal(err)
		}
		p := progressOf(nodes, replyAnchor{bound: "u1"})
		if !p.found {
			t.Fatalf("no reply found: %+v", p)
		}
		return p.sig
	}
	if sig() == sig(copilotSource{Title: "S", URL: "https://s.example/"}) {
		t.Fatal("a source landing after the text left the reply signature unchanged")
	}
}
