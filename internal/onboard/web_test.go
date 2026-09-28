package onboard

import (
	"strings"
	"testing"
)

// Web agents are Go services like history: no model instructions to paste,
// a fixed per-site config path matching tincan web install's service, and
// setup that says they act as the owner in the site.
func TestWebAgentKinds(t *testing.T) {
	for kind, site := range map[string]string{KindChatGPTWeb: "chatgpt", KindClaudeWeb: "claude-ai", KindGrokWeb: "grok", KindGeminiWeb: "gemini", KindPerplexityWeb: "perplexity"} {
		k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: kind, Wake: "wait"}}})
		a := block(t, k, kind)
		if a.Kind != kind || a.Wake != "wait" {
			t.Fatalf("%s: kind/wake = %s/%s (runtime name should map to the kind)", kind, a.Kind, a.Wake)
		}
		cfg := "TINCAN_CONFIG=~/.config/tincan/" + kind + ".json "
		if want := cfg + "tincan join <code> --relay " + relayURL; a.Join != want {
			t.Errorf("%s join = %q, want %q", kind, a.Join, want)
		}
		if strings.Contains(a.Instructions, "check_inbox") {
			t.Errorf("%s instructions carry model guidance:\n%s", kind, a.Instructions)
		}
		setup := strings.Join(a.Setup, "\n")
		for _, want := range []string{
			"tincan web install --site " + site,
			"logged in",
			"as Matt",
			kind + "-allow.txt",
			`"new chat"`, `"conversation: <id>"`,
			`method "wait"`,
			cfg + "tincan rejoin --relay " + relayURL + " --name " + kind,
			"tincan history install",
		} {
			if !strings.Contains(setup, want) {
				t.Errorf("%s setup missing %q:\n%s", kind, want, setup)
			}
		}
		r := recipe(t, k, kind)
		all := strings.Join(r.Steps, "\n")
		if !strings.Contains(all, "tincan invite <name> --kind "+kind) || !strings.Contains(all, cfg+"tincan join") {
			t.Errorf("%s recipe should invite then join with its own config:\n%s", kind, all)
		}
		if r.Title == "" {
			t.Errorf("%s recipe has no title", kind)
		}
	}
	if !KnownKind(KindChatGPTWeb) || !KnownKind(KindClaudeWeb) || !KnownKind(KindGrokWeb) || !KnownKind(KindGeminiWeb) || !KnownKind(KindPerplexityWeb) {
		t.Fatal("web kinds not known to the relay's kind check")
	}
}

// grok-web is set up like the other web agents, plus the grant for its
// optional site and the account-risk note; the others carry neither.
func TestGrokWebSetup(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "grok-web", Wake: "wait"}, {Name: "chatgpt-web", Wake: "wait"}}})
	a := block(t, k, "grok-web")
	if a.Kind != KindGrokWeb || !strings.Contains(a.Instructions, "Grok (grok.com)") {
		t.Fatalf("kind %s instructions %q", a.Kind, a.Instructions)
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"logged in to grok.com",
		"click Grant for Grok",
		"assets.grok.com",
		"tincan web serve --site grok waits for the grant",
		"Tell Matt first: xAI's terms prohibit automated access",
		"Go on only if Matt accepts that.",
		"A step for Matt (Chrome takes the grant only from a click in the browser)",
		"acts as Matt on Matt's own account",
		"com.agenttincan.web.grok.plist",
		"~/.config/tincan/grok-web-allow.txt",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("grok-web setup missing %q:\n%s", want, setup)
		}
	}
	if r := recipe(t, k, KindGrokWeb); r.Title != "Grok (grok.com) as a teammate, through your logged-in browser" {
		t.Errorf("title %q", r.Title)
	}
	other := strings.Join(block(t, k, "chatgpt-web").Setup, "\n")
	if strings.Contains(other, "Grant for") || strings.Contains(other, "xAI") {
		t.Errorf("chatgpt-web setup carries grok steps:\n%s", other)
	}
}

// gemini-web's setup carries what is particular to Gemini: the Google
// account risk, granting the site on the extension's options page, and the
// reach into connected Google apps that makes an allowlist advisable.
func TestGeminiWebSetup(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "gemini-web", Wake: "wait"}}})
	a := block(t, k, KindGeminiWeb)
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"gemini.google.com",
		"options page",
		"Gmail, Drive and Calendar",
		"gemini-web-allow.txt",
		"Google's terms",
		"whole Google account",
		"com.agenttincan.web.gemini.plist",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("gemini-web setup missing %q:\n%s", want, setup)
		}
	}
	if !strings.Contains(a.Instructions, "Gemini (gemini.google.com)") {
		t.Errorf("instructions: %s", a.Instructions)
	}
	if r := recipe(t, k, KindGeminiWeb); !strings.Contains(r.Title, "Gemini") {
		t.Errorf("title %q", r.Title)
	}
	// The other web agents' setup does not carry Gemini's notes.
	c := block(t, build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "claude-web", Wake: "wait"}}}), KindClaudeWeb)
	if strings.Contains(strings.Join(c.Setup, "\n"), "Gmail") {
		t.Error("claude-web setup carries the Gemini data note")
	}
}

// perplexity-web's setup carries what is particular to Perplexity: the
// grant on the options page, the signed-in check, the account risk and
// the sources in its replies; the other web agents carry none of it.
func TestPerplexityWebSetup(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "perplexity-web", Wake: "wait"}, {Name: "grok-web", Wake: "wait"}}})
	a := block(t, k, KindPerplexityWeb)
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{
		"logged in to www.perplexity.ai",
		"Grant next to Perplexity",
		"tincan web serve --site perplexity waits",
		"refuses to send anonymously",
		"Perplexity's terms",
		"Matt's own account",
		"com.agenttincan.web.perplexity.plist",
		"~/.config/tincan/perplexity-web-allow.txt",
		"Sources:",
		"not a history source",
	} {
		if !strings.Contains(setup, want) {
			t.Errorf("perplexity-web setup missing %q:\n%s", want, setup)
		}
	}
	if !strings.Contains(a.Instructions, "Perplexity (www.perplexity.ai)") || !strings.Contains(a.Instructions, "source links") || strings.Contains(a.Instructions, "generated images") {
		t.Errorf("instructions: %s", a.Instructions)
	}
	if r := recipe(t, k, KindPerplexityWeb); !strings.Contains(r.Title, "Perplexity") {
		t.Errorf("title %q", r.Title)
	}
	other := strings.Join(block(t, k, "grok-web").Setup, "\n")
	if strings.Contains(other, "Perplexity") {
		t.Errorf("grok-web setup carries Perplexity steps:\n%s", other)
	}
}
