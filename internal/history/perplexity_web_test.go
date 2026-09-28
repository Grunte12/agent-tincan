package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// perplexityBrowser plays the extension for perplexity-web:
// perplexity.send opens a thread (or continues one), and perplexity.detail
// shows each sent question as a thread entry in the shape of GET
// /rest/thread/<slug>. A new entry is invisible for lag polls, then
// PENDING with half its answer for pending polls, then COMPLETED with its
// markdown still IN_PROGRESS for streaming polls, then done.
type perplexityBrowser struct {
	mu      sync.Mutex
	ops     []Op
	sends   []OpArgs
	closes  []string
	threads map[string][]*pxTurn
	seq     int

	lag, pending, streaming int
	// sources are the web results every answer carries.
	sources []map[string]any
	// sendErr fails every send with that code; detailErr every detail
	// read, with detailRetry as its Retry-After.
	sendErr, detailErr string
	detailRetry        int
	// maxEntries, when set, is the extension's page cap: a longer thread
	// shows only its first maxEntries entries, with more set.
	maxEntries int
	// repeat, when set, answers with the question repeated that many
	// times, for answers longer than a message may be.
	repeat int
}

type pxTurn struct {
	id, query, answer       string
	at                      time.Time
	lag, pending, streaming int
}

func newPerplexityBrowser() *perplexityBrowser {
	return &perplexityBrowser{threads: map[string][]*pxTurn{}, pending: 1, streaming: 1}
}

func (p *perplexityBrowser) count(op Op) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, o := range p.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (p *perplexityBrowser) sent() []OpArgs {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]OpArgs(nil), p.sends...)
}

func (p *perplexityBrowser) closed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.closes...)
}

func (p *perplexityBrowser) Exchange(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	p.mu.Lock()
	p.ops = append(p.ops, req.Op)
	p.mu.Unlock()
	fail := func(code string, retry int) error {
		_, err := recv(NativeResponse{ID: req.ID, Error: &NativeError{Code: code, Message: "fake " + code, RetryAfter: retry}})
		return err
	}
	if err := ValidateOp(req.Op, req.Args); err != nil {
		return fail("bad_request", 0)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch req.Op {
	case OpPerplexitySend:
		p.sends = append(p.sends, req.Args)
		if p.sendErr != "" {
			return fail(p.sendErr, 0)
		}
		slug := req.Args.ConversationID
		if slug == "" {
			p.seq++
			slug = fmt.Sprintf("0e1d0000-0000-4000-8000-%012d", 100+p.seq)
		} else if _, ok := p.threads[slug]; !ok {
			return fail("not_found", 0)
		}
		n := len(p.threads[slug])
		now := time.Now()
		said := req.Args.Message
		if p.repeat > 0 {
			said = strings.Repeat(said, p.repeat)
		}
		p.threads[slug] = append(p.threads[slug], &pxTurn{
			id: fmt.Sprintf("0e1d0000-0000-4000-9000-%09d%03d", 100+p.seq, n), query: req.Args.Message,
			answer: "Perplexity says: " + said + " [1]",
			at:     now, lag: p.lag, pending: p.pending, streaming: p.streaming,
		})
		res, _ := json.Marshal(SendResult{ConversationID: slug, URL: "https://www.perplexity.ai/search/" + slug, SubmittedAt: now.UnixMilli()})
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: res})
		return err
	case OpPerplexityDetail:
		if p.detailErr != "" {
			return fail(p.detailErr, p.detailRetry)
		}
		turns, ok := p.threads[req.Args.ID]
		if !ok {
			return fail("not_found", 0)
		}
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: p.render(req.Args.ID, turns)})
		return err
	case OpPerplexityClose:
		p.closes = append(p.closes, req.Args.ConversationID)
		_, err := recv(NativeResponse{ID: req.ID, OK: true, Result: json.RawMessage(`{"closed":1}`)})
		return err
	}
	return fail("bad_request", 0)
}

// render is called with p.mu held; each call is one poll.
func (p *perplexityBrowser) render(slug string, turns []*pxTurn) json.RawMessage {
	entries := []map[string]any{}
	for _, t := range turns {
		if t.lag > 0 {
			t.lag--
			continue
		}
		status, progress, answer := "COMPLETED", "DONE", t.answer
		switch {
		case t.pending > 0:
			t.pending--
			status, progress, answer = "PENDING", "IN_PROGRESS", t.answer[:len(t.answer)/2]
		case t.streaming > 0:
			t.streaming--
			progress = "IN_PROGRESS"
		}
		results := p.sources
		if results == nil {
			results = []map[string]any{}
		}
		entries = append(entries, map[string]any{
			"uuid": t.id, "backend_uuid": t.id, "status": status, "query_str": t.query, "thread_url_slug": slug,
			"entry_created_datetime": t.at.Add(300 * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000000"),
			"blocks": []map[string]any{
				{"intended_usage": "web_results", "web_result_block": map[string]any{"progress": progress, "web_results": results}},
				{"intended_usage": "ask_text", "markdown_block": map[string]any{"progress": progress, "chunks": []string{answer}, "answer": answer}},
			},
		})
	}
	out := map[string]any{"slug": slug, "entries": entries}
	if p.maxEntries > 0 && len(entries) > p.maxEntries {
		out["entries"], out["more"] = entries[:p.maxEntries], true
	}
	b, _ := json.Marshal(out)
	return b
}

// perplexityRig is the web rig with its agent fronting www.perplexity.ai.
func perplexityRig(t *testing.T) (*webRig, *perplexityBrowser) {
	t.Helper()
	rig := newWebRig(t)
	p := newPerplexityBrowser()
	rig.agent.Site = SourcePerplexity
	rig.agent.Name = "perplexity-web"
	rig.agent.Native = &Client{Channel: p, Cooldown: &SiteCooldown{}}
	return rig, p
}

func pxSources(n int) []map[string]any {
	var out []map[string]any
	for i := range n {
		out = append(out, map[string]any{"name": fmt.Sprintf("Source %d", i+1), "url": fmt.Sprintf("https://example.com/%d", i+1)})
	}
	return out
}

// A send polls until the entry is COMPLETED and its answer DONE, then
// replies with the answer text, the numbered sources and the Perplexity
// conversation footer, and closes the tab.
func TestPerplexityWebReplyWithSourcesAndFooter(t *testing.T) {
	rig, p := perplexityRig(t)
	p.pending, p.streaming = 2, 1
	p.sources = append(pxSources(2), map[string]any{"name": "Picture", "url": "https://example.com/pic.png", "is_image": true})
	res := rig.ask(t, "codex", "What is a tin can telephone?")
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	id := "0e1d0000-0000-4000-8000-000000000101"
	want := "Perplexity says: What is a tin can telephone? [1]\n\nSources:\n- [1] Source 1 https://example.com/1\n- [2] Source 2 https://example.com/2\n\nPerplexity conversation: " + id
	if res.Reply.Body != want {
		t.Fatalf("reply:\n%s\nwant:\n%s", res.Reply.Body, want)
	}
	if n := p.count(OpPerplexityDetail); n != 4 {
		t.Errorf("detail read %d times, want 4 (2 pending, 1 streaming, then done)", n)
	}
	if got := p.closed(); len(got) != 1 || got[0] != id {
		t.Errorf("closes = %v", got)
	}
	if used := loadWebUsed(rig.agent.UsedPath); !used[id] {
		t.Fatalf("conversation not recorded in the used list: %v", used)
	}
}

// More than 10 sources are capped with a count of the rest; a URL listed
// twice appears once.
func TestPerplexityWebSourcesCappedAndDeduped(t *testing.T) {
	rig, p := perplexityRig(t)
	p.sources = append(pxSources(13), map[string]any{"name": "Again", "url": "https://example.com/2"})
	res := rig.ask(t, "codex", "many sources")
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "- [10] Source 10 https://example.com/10\n(and 3 more)\n\nPerplexity conversation:") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if strings.Count(body, "https://example.com/2\n") != 1 || strings.Contains(body, "Again") || strings.Contains(body, "Source 11") {
		t.Fatalf("duplicate or over-cap source listed:\n%s", body)
	}
}

// An answer with no sources has no Sources block.
func TestPerplexityWebNoSources(t *testing.T) {
	rig, _ := perplexityRig(t)
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusAnswered || strings.Contains(res.Reply.Body, "Sources:") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
}

// A thread longer than the extension reads: a wait that loses sight of
// its turn fails at once instead of timing out; a remembered long thread
// is left for a new chat, and a named one is refused before sending.
func TestPerplexityWebThreadTooLong(t *testing.T) {
	rig, p := perplexityRig(t)
	p.maxEntries = 1
	id := "0e1d0000-0000-4000-8000-000000000101"
	if res := rig.ask(t, "codex", "first"); res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	res := rig.ask(t, "codex", "second")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "the message was sent to Perplexity, but the conversation "+id+" is now longer than tincan reads") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if n := p.count(OpPerplexityDetail); n > 5 {
		t.Errorf("detail read %d times; the wait should stop at the first capped read (3 for the first ask, 1 before the send, 1 capped)", n)
	}
	res = rig.ask(t, "codex", "third")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Your previous Perplexity conversation (id "+id+") is longer than tincan reads, so this went to a new chat.") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if s := p.sent(); len(s) != 3 || s[2].ConversationID != "" || !s[2].NewChat {
		t.Fatalf("sends = %+v", s)
	}
	res = rig.ask(t, "codex", "conversation: "+id+"\nfourth")
	if res.Status != envelope.StatusFailed || !strings.HasPrefix(res.Reply.Body, "Nothing was sent to Perplexity: the conversation "+id+" is longer than tincan reads") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if n := len(p.sent()); n != 3 {
		t.Fatalf("%d sends, want 3", n)
	}
}

// A source list too long for the cap never panics the reply: overlong
// URLs are left out, and a negative text budget caps the text to nothing.
func TestPerplexityWebOversizedSources(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", maxSourceURL)
	footer := sourcesFooter([]webSource{{n: 1, title: "Long", url: long}, {n: 2, title: "Short", url: "https://example.com/s"}})
	if strings.Contains(footer, "Long") || footer != "Sources:\n- [2] Short https://example.com/s" {
		t.Fatalf("footer %q", footer)
	}
	if got, cut := capReplyTo("some text", -10); got != "" || !cut {
		t.Fatalf("negative budget: %q %v", got, cut)
	}
	if got := capBytes("abc", -1); got != "" {
		t.Fatalf("capBytes(-1) = %q", got)
	}
	rig, p := perplexityRig(t)
	var srcs []map[string]any
	for i := range 12 {
		srcs = append(srcs, map[string]any{"name": fmt.Sprintf("S%d", i), "url": fmt.Sprintf("https://example.com/%d/%s", i, strings.Repeat("b", 8000))})
	}
	p.sources = srcs
	if res := rig.ask(t, "codex", "huge sources"); res.Status != envelope.StatusAnswered || strings.Contains(res.Reply.Body, "Sources:") {
		t.Fatalf("%s %d bytes", res.Status, len(res.Reply.Body))
	}
}

// A long answer gives way so the whole reply (the cut text, the
// truncation notice, the sources list, a note and the conversation
// footer) stays inside the cap.
func TestPerplexityWebSourcesKeptInsideCap(t *testing.T) {
	rig, p := perplexityRig(t)
	p.sources = pxSources(3)
	res := rig.ask(t, "codex", strings.Repeat("x", 30<<10))
	body := res.Reply.Body
	if res.Status != envelope.StatusAnswered || !strings.Contains(body, "- [3] Source 3 https://example.com/3") {
		t.Fatalf("%s %d bytes", res.Status, len(body))
	}
	// An answer over the cap.
	p.repeat = 3
	res = rig.ask(t, "codex", strings.Repeat("y", 30<<10))
	body = res.Reply.Body
	if res.Status != envelope.StatusAnswered {
		t.Fatalf("%s %q", res.Status, body[:min(len(body), 200)])
	}
	for _, part := range []string{"(reply truncated: showing ", "- [3] Source 3 https://example.com/3", "\n\nPerplexity conversation: "} {
		if !strings.Contains(body, part) {
			t.Fatalf("reply lacks %q:\n%s", part, body[max(len(body)-600, 0):])
		}
	}
	if len(body) > maxWebReplyBytes {
		t.Fatalf("reply is %d bytes, over the %d byte cap", len(body), maxWebReplyBytes)
	}
}

// Threading lines and per-asker conversations, with www.perplexity.ai URLs.
func TestPerplexityWebThreading(t *testing.T) {
	rig, p := perplexityRig(t)
	rig.ask(t, "grokbot", "first")           // new: ...101
	rig.ask(t, "grokbot", "second")          // continues 101
	rig.ask(t, "codex", "codex's own")       // new for codex: 102
	rig.ask(t, "grokbot", "new chat\nthird") // new: 103
	rig.ask(t, "grokbot", "fourth")          // continues 103
	res := rig.ask(t, "codex", "conversation: https://www.perplexity.ai/search/0e1d0000-0000-4000-8000-000000000101\npeek")
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "Perplexity says: peek") || !strings.Contains(res.Reply.Body, "Perplexity conversation: 0e1d0000-0000-4000-8000-000000000101") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	c := func(n int) string { return fmt.Sprintf("0e1d0000-0000-4000-8000-%012d", n) }
	want := []OpArgs{
		{Message: "first"},
		{Message: "second", ConversationID: c(101)},
		{Message: "codex's own"},
		{Message: "third", NewChat: true},
		{Message: "fourth", ConversationID: c(103)},
		{Message: "peek", ConversationID: c(101)},
	}
	got := p.sent()
	if len(got) != len(want) {
		t.Fatalf("sends %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("send %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A 429 while reading the answer (past the request's budget) fails saying
// the message was already sent, and holds back Perplexity only.
func TestPerplexityWebRateLimitIsPerplexityOnly(t *testing.T) {
	rig, p := perplexityRig(t)
	cd := &SiteCooldown{}
	rig.agent.Native.Cooldown = cd
	p.detailErr, p.detailRetry = "rate_limited", 3600
	res := rig.ask(t, "codex", "hello")
	body := res.Reply.Body
	if res.Status != envelope.StatusFailed || !strings.Contains(body, "Perplexity is rate-limiting this account") || !strings.Contains(body, "The message was sent to Perplexity (conversation 0e1d0000-0000-4000-8000-000000000101)") {
		t.Fatalf("%s %q", res.Status, body)
	}
	if cd.Remaining(SourcePerplexity) <= 0 || cd.Remaining(SourceChatGPT) != 0 || cd.Remaining(SourceGrok) != 0 {
		t.Fatalf("cooldowns: perplexity %s chatgpt %s", cd.Remaining(SourcePerplexity), cd.Remaining(SourceChatGPT))
	}
	res = rig.ask(t, "codex", "again")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, "Perplexity is rate-limiting this account right now; try again later") || len(p.sent()) != 1 {
		t.Fatalf("%s %q sends %d", res.Status, res.Reply.Body, len(p.sent()))
	}
	chat := newWebRig(t)
	chat.agent.Native.Cooldown = cd
	if res := chat.ask(t, "codex", "still fine?"); res.Status != envelope.StatusAnswered {
		t.Fatalf("chatgpt during a perplexity cooldown: %s %q", res.Status, res.Reply.Body)
	}
}

// Logged out (the extension's session gate or the page's login probe),
// an anti-bot page and a missing grant each name their cause and fix,
// and a failed send leaves no tab to close. An anti-bot page also holds
// Perplexity back for a while.
func TestPerplexityWebSendFailureReplies(t *testing.T) {
	for code, want := range map[string]string{
		"not_logged_in":      "not logged in to www.perplexity.ai in Chrome",
		"blocked":            "www.perplexity.ai showed an anti-bot check; open www.perplexity.ai in Chrome, complete the check",
		"permission_missing": "has no access to www.perplexity.ai; grant it on the extension's options page",
	} {
		rig, p := perplexityRig(t)
		cd := &SiteCooldown{}
		rig.agent.Native.Cooldown = cd
		p.sendErr = code
		res := rig.ask(t, "codex", "hello")
		if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, want) {
			t.Errorf("%s: %s %q", code, res.Status, res.Reply.Body)
		}
		if got := p.closed(); len(got) != 0 {
			t.Errorf("%s: closes %v", code, got)
		}
		if n := p.count(OpPerplexityDetail); n != 0 {
			t.Errorf("%s: %d detail reads after a failed send", code, n)
		}
		if (code == "blocked") != (cd.Remaining(SourcePerplexity) > 0) {
			t.Errorf("%s: cooldown %s", code, cd.Remaining(SourcePerplexity))
		}
	}
}
