package council

import (
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// team is a roster with one agent of every shape eligibility tells apart.
var team = []onboard.Member{
	{Name: "codex", Kind: "codex", Wake: "command"},
	{Name: "gemini-cli", Kind: "gemini-cli", Wake: "command"},
	{Name: "claude-web", Kind: "claude-web", Wake: "wait"},
	{Name: "chatgpt-web", Kind: "chatgpt-web", Wake: "wait"},
	{Name: "gemini-web", Kind: "gemini-web", Wake: "wait"},
	{Name: "grok-web", Kind: "grok-web", Wake: "wait"},
	{Name: "hermes", Kind: "hermes", Wake: "webhook"},
	{Name: "claude-code", Kind: "claude-code", Wake: "channel"},
	{Name: "history", Kind: "history", Wake: "wait"},
	{Name: "notes", Kind: "notes", Wake: "wait"},
	{Name: "council", Kind: "council", Wake: "wait"},
	{Name: "cron", Kind: "scheduled", Wake: "schedule"},
	{Name: "chatgpt", Kind: "chatgpt", Wake: "none"},
}

// owner is a convening request straight from the owner's CLI.
var owner = envelope.Request{From: "matt", Chain: []string{"matt"}, Hop: 1}

func question(t *testing.T, body string, cfg Config) Request {
	t.Helper()
	r, err := ParseRequest(body, cfg)
	if err != nil {
		t.Fatalf("%q: %v", body, err)
	}
	return r
}

func reasons(e Eligibility) map[string]string {
	out := map[string]string{}
	for _, x := range e.Excluded {
		out[x.Name] = x.Reason
	}
	return out
}

func TestFreeTextUsesDefaultRosterAndChairmen(t *testing.T) {
	cfg := DefaultConfig()
	e := Resolve(team, owner, cfg, question(t, "Should we shard the store?", cfg))
	if e.Decline != "" {
		t.Fatalf("declined: %s", e.Decline)
	}
	want := []string{"codex", "gemini-cli", "claude-web", "chatgpt-web", "gemini-web", "grok-web", "hermes"}
	if !slices.Equal(e.Members, want) {
		t.Fatalf("members %v, want %v", e.Members, want)
	}
	if want := []string{"claude-web", "chatgpt-web", "gemini-web"}; !slices.Equal(e.Chairmen, want) {
		t.Fatalf("chairmen %v, want %v", e.Chairmen, want)
	}
	got := reasons(e)
	for name, reason := range map[string]string{
		"claude-code": "excluded (live session)",
		"history":     "excluded (service)",
		"notes":       "excluded (service)",
		"council":     "excluded (service)",
		"cron":        "excluded (service)",
		"chatgpt":     "excluded (not wakeable)",
	} {
		if got[name] != reason {
			t.Errorf("%s: reason %q, want %q", name, got[name], reason)
		}
	}
}

func TestFormMembersAndChairmanAreUsed(t *testing.T) {
	cfg := DefaultConfig()
	r := question(t, `council: {"question":"Which cache?","members":["codex","grok-web","gemini-web"],"chairman":"grok-web"}`, cfg)
	e := Resolve(team, owner, cfg, r)
	if e.Decline != "" {
		t.Fatalf("declined: %s", e.Decline)
	}
	if want := []string{"codex", "gemini-web", "grok-web"}; !slices.Equal(e.Members, want) {
		t.Fatalf("members %v, want %v", e.Members, want)
	}
	if want := []string{"grok-web", "claude-web", "chatgpt-web", "gemini-web"}; !slices.Equal(e.Chairmen, want) {
		t.Fatalf("chairmen %v, want %v", e.Chairmen, want)
	}
}

// AE6: Codex convenes from inside a chain that started with planner.
func TestConvenerAndChainAreExcludedInChain(t *testing.T) {
	roster := append(slices.Clone(team), onboard.Member{Name: "planner", Kind: "openclaw", Wake: "webhook"})
	conv := envelope.Request{From: "codex", Chain: []string{"planner", "codex"}, Hop: 2}
	cfg := DefaultConfig()
	e := Resolve(roster, conv, cfg, question(t, "Is this plan sound?", cfg))
	if e.Decline != "" {
		t.Fatalf("declined: %s", e.Decline)
	}
	got := reasons(e)
	for _, name := range []string{"codex", "planner"} {
		if got[name] != "excluded (in chain)" {
			t.Errorf("%s: reason %q, want excluded (in chain)", name, got[name])
		}
		if slices.Contains(e.Members, name) || slices.Contains(e.Chairmen, name) {
			t.Errorf("%s still seated: members %v chairmen %v", name, e.Members, e.Chairmen)
		}
	}
}

func TestClaudeCodeOnlyWhenOwnerListsIt(t *testing.T) {
	cfg := DefaultConfig()
	r := question(t, "Review this diff", cfg)
	if e := Resolve(team, owner, cfg, r); slices.Contains(e.Members, "claude-code") {
		t.Fatalf("claude-code on the default roster: %v", e.Members)
	}
	cfg.Members = []string{"claude-code"}
	if e := Resolve(team, owner, cfg, r); !slices.Contains(e.Members, "claude-code") {
		t.Fatalf("listed claude-code missing: %v", e.Members)
	}
}

func TestTooFewEligibleDeclines(t *testing.T) {
	roster := []onboard.Member{
		{Name: "codex", Kind: "codex", Wake: "command"},
		{Name: "claude-web", Kind: "claude-web", Wake: "wait"},
		{Name: "chatgpt-web", Kind: "chatgpt-web", Wake: "wait"},
		{Name: "history", Kind: "history", Wake: "wait"},
	}
	conv := envelope.Request{From: "codex", Chain: []string{"codex"}, Hop: 1}
	cfg := DefaultConfig()
	e := Resolve(roster, conv, cfg, question(t, "Tabs or spaces?", cfg))
	if e.Decline == "" || !strings.Contains(e.Decline, "3") {
		t.Fatalf("decline %q, want one naming the minimum of 3 (members %v)", e.Decline, e.Members)
	}
	// The same roster convened by the owner seats all three.
	e = Resolve(roster, owner, cfg, question(t, "Tabs or spaces?", cfg))
	if e.Decline != "" {
		t.Fatalf("3 eligible declined: %s", e.Decline)
	}
}

func TestHopLimit(t *testing.T) {
	cfg := DefaultConfig()
	r := question(t, "Deep?", cfg)
	deep := envelope.Request{From: "codex", Chain: []string{"a", "b", "c", "codex"}, Hop: 4}
	if e := Resolve(team, deep, cfg, r); !strings.Contains(e.Decline, "too deep") {
		t.Fatalf("hop 4: decline %q, want too deep", e.Decline)
	}
	ok := envelope.Request{From: "codex", Chain: []string{"a", "b", "codex"}, Hop: 3}
	if e := Resolve(team, ok, cfg, r); e.Decline != "" {
		t.Fatalf("hop 3 declined: %s", e.Decline)
	}
}

func TestChairmanInChainIsSkipped(t *testing.T) {
	conv := envelope.Request{From: "claude-web", Chain: []string{"claude-web"}, Hop: 1}
	cfg := DefaultConfig()
	e := Resolve(team, conv, cfg, question(t, "Which?", cfg))
	if want := []string{"chatgpt-web", "gemini-web"}; !slices.Equal(e.Chairmen, want) {
		t.Fatalf("chairmen %v, want %v", e.Chairmen, want)
	}
}

func TestChairmanCandidateMatchesKind(t *testing.T) {
	roster := []onboard.Member{
		{Name: "claude", Kind: "claude-web", Wake: "wait"},
		{Name: "gpt", Kind: "chatgpt-web", Wake: "wait"},
		{Name: "codex", Kind: "codex", Wake: "command"},
	}
	cfg := DefaultConfig()
	e := Resolve(roster, owner, cfg, question(t, "Which?", cfg))
	if want := []string{"claude", "gpt"}; !slices.Equal(e.Chairmen, want) {
		t.Fatalf("chairmen %v, want %v", e.Chairmen, want)
	}
}

func TestFormNamingServiceOrLiveSessionDeclines(t *testing.T) {
	for _, body := range []string{
		`council: {"question":"q","members":["codex","gemini-web","history"]}`,
		`council: {"question":"q","members":["codex","gemini-web","claude-code"]}`,
		`council: {"question":"q","chairman":"history"}`,
		`council: {"question":"q","chairman":"claude-code"}`,
		`council: {"question":"q","chairman":"nobody"}`,
	} {
		cfg := DefaultConfig()
		e := Resolve(team, owner, cfg, question(t, body, cfg))
		if e.Decline == "" {
			t.Errorf("%s: not declined (members %v, chairmen %v)", body, e.Members, e.Chairmen)
		}
		listed := DefaultConfig()
		listed.Members = []string{"history", "claude-code"}
		if strings.Contains(body, "nobody") {
			continue
		}
		if e := Resolve(team, owner, listed, question(t, body, listed)); e.Decline != "" {
			t.Errorf("%s: declined although the owner listed it: %s", body, e.Decline)
		}
	}
	// Listing an agent as a chairman candidate in council.json also counts.
	cfg := DefaultConfig()
	cfg.Chairmen = []string{"claude-code", "claude-web"}
	e := Resolve(team, owner, cfg, question(t, `council: {"question":"q","chairman":"claude-code"}`, cfg))
	if e.Decline != "" || e.Chairmen[0] != "claude-code" {
		t.Fatalf("listed chairman: decline %q, chairmen %v", e.Decline, e.Chairmen)
	}
}

func TestEveryExclusionReasonHasItsOwnLabel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Exclude = []string{"grok-web"}
	conv := envelope.Request{From: "codex", Chain: []string{"codex"}, Hop: 1}
	e := Resolve(team, conv, cfg, question(t, `council: {"question":"q","exclude":["hermes"]}`, cfg))
	got := reasons(e)
	want := map[string]string{
		"codex":       "excluded (in chain)",
		"claude-code": "excluded (live session)",
		"history":     "excluded (service)",
		"chatgpt":     "excluded (not wakeable)",
		"grok-web":    "excluded (by owner)",
		"hermes":      "excluded (by request)",
	}
	labels := map[string]bool{}
	for name, reason := range want {
		if got[name] != reason {
			t.Errorf("%s: reason %q, want %q", name, got[name], reason)
		}
		labels[reason] = true
	}
	if len(labels) != len(want) {
		t.Fatalf("labels not distinct: %v", labels)
	}
	if want := []string{"gemini-cli", "claude-web", "chatgpt-web", "gemini-web"}; !slices.Equal(e.Members, want) {
		t.Fatalf("members %v, want %v", e.Members, want)
	}
}

func TestOwnerExcludedMemberCanBeRequested(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Exclude = []string{"grok-web"}
	e := Resolve(team, owner, cfg, question(t, `council: {"question":"q","members":["grok-web","codex","claude-web"]}`, cfg))
	if e.Decline != "" || !slices.Contains(e.Members, "grok-web") {
		t.Fatalf("decline %q, members %v", e.Decline, e.Members)
	}
}
