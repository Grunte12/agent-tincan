package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// grokBrowser plays the extension for grok-web: grok.send opens a
// conversation (or continues one), and grok.detail shows each sent turn
// as the extension's combined response-node and load-responses answer.
// A new turn is invisible for lag polls, then its answer is partial for
// partial polls, then finished but still listed in flight for inflight
// polls, then finished.
type grokBrowser struct {
	mu     sync.Mutex
	ops    []Op
	sends  []OpArgs
	closes []string
	files  []OpArgs
	convs  map[string][]*grokTurn
	seq    int

	lag, partial, inflight int
	// image, when set, is generated with every answer; imageErr fails its
	// fetch with that code.
	image    []byte
	imageErr string
	// streamErr is put in every answer's streamErrors.
	streamErr string
	// sendErr fails every send with that code; detailErr every detail
	// read.
	sendErr, detailErr string
	detailRetry        int
}

type grokTurn struct {
	human, answer          string
	humanID, answerID      string
	at                     time.Time
	lag, partial, inflight int
	image                  bool
}

func newGrokBrowser() *grokBrowser {
	return &grokBrowser{convs: map[string][]*grokTurn{}, partial: 1}
}

func (g *grokBrowser) count(op Op) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, o := range g.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (g *grokBrowser) sent() []OpArgs {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]OpArgs(nil), g.sends...)
}

func (g *grokBrowser) closed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.closes...)
}

func (g *grokBrowser) Exchange(ctx context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	g.mu.Lock()
	g.ops = append(g.ops, req.Op)
	g.mu.Unlock()
	fail := func(code string, retry int) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: "fake " + code, RetryAfter: retry}})
		return err
	}
	if err := ValidateOp(req.Op, req.Args); err != nil {
		return fail("bad_request", 0)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch req.Op {
	case OpGrokSend:
		g.sends = append(g.sends, req.Args)
		if g.sendErr != "" {
			return fail(g.sendErr, 0)
		}
		id := req.Args.ConversationID
		if id == "" {
			g.seq++
			id = fmt.Sprintf("0e1d0000-0000-4000-8000-%012d", 100+g.seq)
		} else if _, ok := g.convs[id]; !ok {
			return fail("not_found", 0)
		}
		n := len(g.convs[id])
		now := time.Now()
		g.convs[id] = append(g.convs[id], &grokTurn{
			human: req.Args.Message, answer: "Grok says: " + req.Args.Message,
			humanID: fmt.Sprintf("h-%s-%d", id[len(id)-3:], n), answerID: fmt.Sprintf("a-%s-%d", id[len(id)-3:], n),
			at: now, lag: g.lag, partial: g.partial, inflight: g.inflight, image: g.image != nil,
		})
		res, _ := json.Marshal(SendResult{ConversationID: id, URL: "https://grok.com/c/" + id, SubmittedAt: now.UnixMilli()})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
		return err
	case OpGrokDetail:
		if g.detailErr != "" {
			return fail(g.detailErr, g.detailRetry)
		}
		turns, ok := g.convs[req.Args.ID]
		if !ok {
			return fail("not_found", 0)
		}
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: g.render(req.Args.ID, turns)})
		return err
	case OpGrokFile:
		g.files = append(g.files, req.Args)
		if g.imageErr != "" {
			return fail(g.imageErr, 0)
		}
		for _, fr := range chunkFrames(g.image, "image/png", 1<<10) {
			fr.ID = req.ID
			if done, err := recv(fr); done || err != nil {
				return err
			}
		}
		return nil
	case OpGrokClose:
		g.closes = append(g.closes, req.Args.ConversationID)
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
		return err
	}
	return fail("bad_request", 0)
}

// render is called with g.mu held; each call is one poll.
func (g *grokBrowser) render(id string, turns []*grokTurn) json.RawMessage {
	var nodes, responses, inflight []map[string]any
	parent := ""
	for _, t := range turns {
		if t.lag > 0 {
			t.lag--
			continue
		}
		nodes = append(nodes, map[string]any{"responseId": t.humanID, "sender": "human", "parentResponseId": parent})
		responses = append(responses, map[string]any{"responseId": t.humanID, "sender": "human", "message": t.human, "createTime": t.at.Add(300 * time.Millisecond).UTC().Format(time.RFC3339Nano), "parentResponseId": parent, "partial": false})
		parent = t.humanID
		a := map[string]any{"responseId": t.answerID, "sender": "assistant", "createTime": t.at.Add(time.Second).UTC().Format(time.RFC3339Nano), "parentResponseId": t.humanID, "generatedImageUrls": []string{}, "streamErrors": []any{}}
		switch {
		case t.partial > 0:
			t.partial--
			a["message"], a["partial"] = t.answer[:len(t.answer)/2], true
			inflight = append(inflight, map[string]any{"responseId": t.answerID})
		case t.inflight > 0:
			t.inflight--
			a["message"], a["partial"] = t.answer, false
			inflight = append(inflight, map[string]any{"responseId": t.answerID})
		default:
			a["message"], a["partial"] = t.answer, false
			if t.image {
				a["generatedImageUrls"] = []string{"users/00000000-0000-4000-8000-0000000000aa/generated/" + t.answerID + "/image.png"}
			}
			if g.streamErr != "" {
				a["streamErrors"] = []any{map[string]any{"message": g.streamErr}}
			}
		}
		nodes = append(nodes, map[string]any{"responseId": t.answerID, "sender": "assistant", "parentResponseId": t.humanID})
		responses = append(responses, a)
		parent = t.answerID
	}
	if nodes == nil {
		nodes, responses = []map[string]any{}, []map[string]any{}
	}
	if inflight == nil {
		inflight = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"conversationId": id, "responseNodes": nodes, "inflightResponses": inflight, "responses": responses})
	return b
}

// grokRig is the web rig with its agent fronting grok.com.
func grokRig(t *testing.T) (*webRig, *grokBrowser) {
	t.Helper()
	rig := newWebRig(t)
	g := newGrokBrowser()
	rig.agent.Site = SourceGrok
	rig.agent.Name = "grok-web"
	rig.agent.Native = &Client{Channel: g, Cooldown: &SiteCooldown{}}
	return rig, g
}

// A send polls until grok.com marks the answer finished (partial false
// and nothing in flight), then replies with the text, the generated
// image and the Grok conversation footer, and closes the tab.
func TestGrokWebReplyWithImageAndFooter(t *testing.T) {
	rig, g := grokRig(t)
	img := fakePNG(1500)
	g.image, g.partial, g.inflight = img, 2, 2
	res := rig.ask(t, "grokbot", "Draw a fox in a tin can")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	id := "0e1d0000-0000-4000-8000-000000000101"
	body := res.Reply.Body
	for _, want := range []string{"Grok says: Draw a fox in a tin can", "Grok conversation: " + id, "1 image attached."} {
		if !strings.Contains(body, want) {
			t.Errorf("reply missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "could not be attached") {
		t.Errorf("image note on a fetched image:\n%s", body)
	}
	if n := g.count(OpGrokDetail); n != 5 {
		t.Errorf("detail read %d times, want 5 (2 partial, 2 in flight, then finished)", n)
	}
	if got := g.closed(); len(got) != 1 || got[0] != id {
		t.Errorf("closes = %v", got)
	}
	if len(g.files) != 1 || g.files[0].ConversationID != id || g.files[0].FileID != "a-101-0_0" {
		t.Errorf("file reads %+v", g.files)
	}
	if len(res.Reply.Attachments) != 1 {
		t.Fatalf("attachments = %+v", res.Reply.Attachments)
	}
	data, _, err := rig.mesh.Client(t, "grokbot").FetchAttachment(t.Context(), res.Reply.Attachments[0].ID)
	if err != nil || !bytes.Equal(data, img) {
		t.Fatalf("attachment %d bytes %v", len(data), err)
	}
	if used := loadWebUsed(rig.agent.UsedPath); !used[id] {
		t.Fatalf("conversation not recorded for history to skip: %v", used)
	}
}

// An image that cannot be fetched from assets.grok.com is left out and
// the reply says so; the text still comes back.
func TestGrokWebImageFetchFailureAddsNote(t *testing.T) {
	rig, g := grokRig(t)
	g.image, g.imageErr = fakePNG(100), "http_error"
	res := rig.ask(t, "grokbot", "Draw a fox")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "Grok says: Draw a fox") || !strings.Contains(body, "The images could not be attached.") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if len(res.Reply.Attachments) != 0 {
		t.Fatalf("attachments %+v", res.Reply.Attachments)
	}
}

// Threading lines and per-asker conversations, with grok.com URLs.
func TestGrokWebThreading(t *testing.T) {
	rig, g := grokRig(t)
	rig.ask(t, "grokbot", "first")           // new: ...101
	rig.ask(t, "grokbot", "second")          // continues 101
	rig.ask(t, "codex", "codex's own")       // new for codex: 102
	rig.ask(t, "grokbot", "new chat\nthird") // new: 103
	rig.ask(t, "grokbot", "fourth")          // continues 103
	res := rig.ask(t, "codex", "conversation: https://grok.com/c/0e1d0000-0000-4000-8000-000000000101\npeek")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Grok conversation: 0e1d0000-0000-4000-8000-000000000101") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	rig.ask(t, "codex", "back to mine") // codex now continues 101, the one it used last
	c := func(n int) string { return fmt.Sprintf("0e1d0000-0000-4000-8000-%012d", n) }
	want := []OpArgs{
		{Message: "first"},
		{Message: "second", ConversationID: c(101)},
		{Message: "codex's own"},
		{Message: "third", NewChat: true},
		{Message: "fourth", ConversationID: c(103)},
		{Message: "peek", ConversationID: c(101)},
		{Message: "back to mine", ConversationID: c(101)},
	}
	got := g.sent()
	if len(got) != len(want) {
		t.Fatalf("sends %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("send %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A 429 while reading Grok's answer (past the request's budget) fails
// saying the message was already sent, and holds back Grok only: a
// ChatGPT agent on the same cooldown keeps working, and the next Grok
// request fails before anything is sent.
func TestGrokWebRateLimitIsGrokOnly(t *testing.T) {
	rig, g := grokRig(t)
	cd := &SiteCooldown{}
	rig.agent.Native.Cooldown = cd
	g.detailErr, g.detailRetry = "rate_limited", 3600
	res := rig.ask(t, "grokbot", "hello")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || !strings.Contains(body, "Grok is rate-limiting this account") || !strings.Contains(body, "The message was sent to Grok (conversation 0e1d0000-0000-4000-8000-000000000101)") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if cd.Remaining(SourceGrok) <= 0 || cd.Remaining(SourceChatGPT) != 0 {
		t.Fatalf("cooldowns: grok %s chatgpt %s", cd.Remaining(SourceGrok), cd.Remaining(SourceChatGPT))
	}
	res = rig.ask(t, "grokbot", "again")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "Grok is rate-limiting this account right now; try again later") || len(g.sent()) != 1 {
		t.Fatalf("%s %q sends %d", res.Status, res.Reply.Body, len(g.sent()))
	}
	chat := newWebRig(t)
	chat.agent.Native.Cooldown = cd
	if res := chat.ask(t, "grokbot", "still fine?"); res.Status != envelope.StatusAnswered {
		t.Fatalf("chatgpt during a grok cooldown: %s %q", res.Status, res.Reply.Body)
	}
}

// An answer that ends on the plan's limit (a stream error) is not
// returned as the reply: the asker hears the message was sent, and Grok
// alone is held back.
func TestGrokWebPlanLimitSetsGrokCooldown(t *testing.T) {
	rig, g := grokRig(t)
	cd := &SiteCooldown{}
	rig.agent.Native.Cooldown = cd
	g.streamErr = "You have reached your usage limit. Upgrade for more."
	res := rig.ask(t, "grokbot", "hello")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || !strings.Contains(body, "Grok is rate-limiting this account") || !strings.Contains(body, "The message was sent to Grok") || strings.Contains(body, "Grok says") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if left := cd.Remaining(SourceGrok); left < DefaultPlanLimitCooldown-time.Minute || cd.Remaining(SourceChatGPT) != 0 {
		t.Fatalf("cooldowns: grok %s chatgpt %s", left, cd.Remaining(SourceChatGPT))
	}
	if got := g.closed(); len(got) != 1 {
		t.Fatalf("closes %v", got)
	}
}

// Logged out, an anti-bot page and a missing grant each name their cause
// and fix; a failed send has no tab to close.
func TestGrokWebSendFailureReplies(t *testing.T) {
	for code, want := range map[string]string{
		"not_logged_in":      "not logged in to grok.com in Chrome",
		"blocked":            "grok.com showed an anti-bot check; open grok.com in Chrome, complete the check",
		"permission_missing": "has no access to grok.com; grant it on the extension's options page",
	} {
		rig, g := grokRig(t)
		g.sendErr = code
		res := rig.ask(t, "grokbot", "hello")
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, want) {
			t.Errorf("%s: %s %q", code, res.Status, res.Reply.Body)
		}
		if got := g.closed(); len(got) != 0 {
			t.Errorf("%s: closes %v", code, got)
		}
	}
}
