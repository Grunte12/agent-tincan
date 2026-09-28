package history

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Every op constant belongs to the site its prefix names.
func TestOpsResolveToTheirOwnSite(t *testing.T) {
	cases := map[Op]Source{
		OpChatGPTList: SourceChatGPT, OpChatGPTDetail: SourceChatGPT, OpChatGPTFile: SourceChatGPT,
		OpChatGPTSend: SourceChatGPT, OpChatGPTClose: SourceChatGPT,
		OpClaudeAIList: SourceClaudeAI, OpClaudeAIDetail: SourceClaudeAI, OpClaudeAIFile: SourceClaudeAI,
		OpClaudeAISend: SourceClaudeAI, OpClaudeAIClose: SourceClaudeAI,
		OpGrokList: SourceGrok, OpGrokDetail: SourceGrok, OpGrokFile: SourceGrok,
		OpGrokSend: SourceGrok, OpGrokClose: SourceGrok,
	}
	for op, want := range cases {
		if got := op.source(); got != want {
			t.Errorf("%s: source %q, want %q", op, got, want)
		}
	}
}

// An op with an unknown prefix, or an unknown verb on a known prefix, is
// not attributed to ChatGPT, and the client refuses it without sending.
func TestUnknownOpIsRejectedNotChatGPT(t *testing.T) {
	for _, op := range []Op{"perplexity.list", "chatgpt.delete", "list", "", OpExtensionReload} {
		if got := op.source(); got != "" {
			t.Errorf("%q: source %q, want none", op, got)
		}
	}
	sent := 0
	c := &Client{Channel: channelFunc(func(context.Context, NativeRequest, func(NativeResponse) (bool, error)) error {
		sent++
		return nil
	}), Cooldown: &SiteCooldown{}}
	_, err := c.Request(context.Background(), "perplexity.list", OpArgs{Count: 1})
	if !errors.Is(err, ErrRejected) || strings.Contains(err.Error(), "chatgpt") {
		t.Fatalf("err = %v, want a rejection not attributed to chatgpt", err)
	}
	if sent != 0 {
		t.Fatalf("%d requests sent for an unknown op", sent)
	}
}

// Send and Close to a site that is not in the table fail instead of
// going to ChatGPT.
func TestSendAndCloseUnknownSiteFail(t *testing.T) {
	var mu sync.Mutex
	var ops []Op
	c := &Client{Channel: channelFunc(func(_ context.Context, req NativeRequest, _ func(NativeResponse) (bool, error)) error {
		mu.Lock()
		ops = append(ops, req.Op)
		mu.Unlock()
		return nil
	}), Cooldown: &SiteCooldown{}}
	if _, err := c.Send(context.Background(), "perplexity", "hi", "", true); err == nil {
		t.Fatal("send to an unknown site succeeded")
	}
	if err := c.Close(context.Background(), "perplexity", "abc-1"); err == nil {
		t.Fatal("close on an unknown site succeeded")
	}
	if len(ops) != 0 {
		t.Fatalf("ops sent: %v", ops)
	}
}

func TestParseWebSiteUnknownListsKnownSites(t *testing.T) {
	for _, s := range WebSites {
		if got, err := ParseWebSite(string(s)); err != nil || got != s {
			t.Fatalf("ParseWebSite(%q) = %q, %v", s, got, err)
		}
	}
	_, err := ParseWebSite("perplexity")
	if err == nil || err.Error() != `unknown site "perplexity" (want chatgpt, claude-ai or grok)` {
		t.Fatalf("err = %v", err)
	}
}

// A cooldown noted for one site holds back only that site.
func TestCooldownsAreIndependentPerSite(t *testing.T) {
	for _, limited := range WebSites {
		cd := &SiteCooldown{}
		cd.Note(limited, time.Minute)
		c := &Client{Channel: frames(), Cooldown: cd}
		for _, s := range WebSites {
			left := c.CooldownRemaining(s)
			if (s == limited) != (left > 0) {
				t.Fatalf("cooldown on %s: %s has %s left", limited, s, left)
			}
		}
		_, err := c.Send(context.Background(), limited, "hi", "", true)
		if !errors.Is(err, ErrRateLimited) || err.Error() != "source unavailable: "+string(limited)+": "+siteLabel(limited)+" is rate-limiting this account right now; try again later" {
			t.Fatalf("send to %s during its cooldown: %v", limited, err)
		}
	}
}

// A web agent for a site outside the table fails its requests instead of
// acting as ChatGPT, and sends nothing.
func TestWebAgentUnknownSiteFails(t *testing.T) {
	rig := newWebRig(t)
	rig.agent.Site = "perplexity"
	res := rig.ask(t, "codex", "hello")
	if res.Status != envelope.StatusFailed || !strings.Contains(res.Reply.Body, `unknown site "perplexity"`) {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if n := rig.browser.exchanges(); n != 0 {
		t.Fatalf("%d extension requests for an unknown site", n)
	}
}

// Only claude.ai has the text-stability rule, with the default windows;
// the agent's overrides change the windows but never give ChatGPT or
// Grok (which mark a finished answer explicitly) one.
func TestStabilityRuleIsClaudeOnly(t *testing.T) {
	for _, s := range []Source{SourceChatGPT, SourceGrok} {
		if r := (&WebAgent{Site: s, ClaudeStablePolls: 2, ClaudeStableFor: time.Second}).stableRule(); r != nil {
			t.Fatalf("%s has a stability rule: %+v", s, *r)
		}
	}
	r := (&WebAgent{Site: SourceClaudeAI}).stableRule()
	if r == nil || r.polls != 4 || r.span != 10*time.Second {
		t.Fatalf("claude.ai rule = %+v", r)
	}
	r = (&WebAgent{Site: SourceClaudeAI, ClaudeStablePolls: 2, ClaudeStableFor: time.Second}).stableRule()
	if r == nil || r.polls != 2 || r.span != time.Second {
		t.Fatalf("claude.ai rule with overrides = %+v", r)
	}
	if siteFor(SourceClaudeAI).stable.polls != DefaultClaudeStablePolls {
		t.Fatal("an override changed the table")
	}
}

// The table keeps each site's names and forms as they were.
func TestSiteTableNames(t *testing.T) {
	if WebSiteNames() != "chatgpt, claude-ai or grok" || WebAgentNames() != "chatgpt-web, claude-web or grok-web" || LiveSourcesLabel() != "ChatGPT, claude.ai and Grok" {
		t.Fatalf("names %q, agents %q, labels %q", WebSiteNames(), WebAgentNames(), LiveSourcesLabel())
	}
	if SourceNames() != "chatgpt, claude-ai, grok, codex or claude-code" {
		t.Fatalf("sources %q", SourceNames())
	}
	want := map[Source][3]string{
		SourceChatGPT:  {"chatgpt-web", "ChatGPT", "chatgpt.com"},
		SourceClaudeAI: {"claude-web", "claude.ai", "claude.ai"},
		SourceGrok:     {"grok-web", "Grok", "grok.com"},
	}
	for src, w := range want {
		if got := [3]string{WebAgentName(src), siteLabel(src), siteOf(src)}; got != w {
			t.Fatalf("%s: %v, want %v", src, got, w)
		}
		if !IsLiveSource(src) {
			t.Fatalf("%s is not live", src)
		}
		r, ok := NewLiveReader(src, &Client{}, nil)
		if !ok || r.Source() != src {
			t.Fatalf("%s reader: %v %v", src, r, ok)
		}
	}
	for _, src := range []Source{SourceCodex, SourceClaudeCode, "perplexity"} {
		if IsLiveSource(src) || WebAgentName(src) != "" {
			t.Fatalf("%s treated as a live site", src)
		}
		if _, ok := NewLiveReader(src, &Client{}, nil); ok {
			t.Fatalf("%s has a live reader", src)
		}
	}
	if joinList([]string{"a", "b", "c"}, "or") != "a, b or c" || joinList([]string{"a"}, "or") != "a" {
		t.Fatal("joinList")
	}
}

// Conversation URLs are read on every site's host and no other.
func TestConversationRefHosts(t *testing.T) {
	for ref, want := range map[string]string{
		"https://chatgpt.com/c/abc-1":                             "abc-1",
		"https://chatgpt.com/g/g-x/c/abc-2":                       "abc-2",
		"https://claude.ai/chat/abc-3":                            "abc-3",
		"https://example.com/c/abc-4":                             "",
		"http://chatgpt.com/c/abc-5":                              "",
		"https://claude.ai.example.com/chat/x":                    "",
		"https://grok.com/c/0e1d0000-0000-4000-8000-000000000001": "0e1d0000-0000-4000-8000-000000000001",
		"https://grok.com/chat/abc-6":                             "",
		"https://grok.com.example.com/c/abc-7":                    "",
	} {
		got, ok := conversationRef(ref)
		if ok != (want != "") || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, ok, want)
		}
	}
}
