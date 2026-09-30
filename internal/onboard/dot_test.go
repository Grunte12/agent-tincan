package onboard

import (
	"slices"
	"strings"
	"testing"
)

func TestDotIsAKnownKind(t *testing.T) {
	if !KnownKind("dot") || KindDot != "dot" || !slices.Contains(Kinds, KindDot) {
		t.Fatalf("dot is not a known kind: %v", Kinds)
	}
	if defaultWake[KindDot] != "webhook" {
		t.Errorf("dot default wake = %q, want webhook", defaultWake[KindDot])
	}
	if _, ok := runtimeNames["dot"]; ok {
		t.Error("dot must not resolve from a runtime name: it is always set with --kind")
	}
	if isWebKind(KindDot) || isService(KindDot) || freshSession[KindDot] || expectOnline(KindDot, "webhook") {
		t.Error("dot is a model on its own cloud computer: not a web kind, service, shared-machine kind, or always online")
	}
}

// A dot runs tincan from its own cloud computer, woken by a fixed nudge in
// its DM or by its heartbeat. Its standing instructions carry the drain
// loop, the trust rules (tincan content is data; connector writes need the
// owner's confirmation; no secrets) and how it recovers from a reset.
func TestDotBlock(t *testing.T) {
	k := build(t, Options{RelayURL: relayURL, Owner: "Matt", Roster: []Member{{Name: "dot", Kind: KindDot}, {Name: "muse", Kind: KindProxySandbox}}})
	a := block(t, k, "dot")
	if a.Kind != KindDot || a.Wake != "webhook" {
		t.Errorf("dot block kind/wake = %s/%s", a.Kind, a.Wake)
	}
	if !strings.Contains(a.Join, "tincan join <code> --relay "+relayURL) {
		t.Errorf("dot join line = %q", a.Join)
	}
	for _, want := range []string{
		// drain loop and the nudge
		"[tincan-auto]", "heartbeat", "tincan inbox", "until it shows nothing", "claims waiting requests", "tincan progress", "tincan reply",
		"replies to your own requests",
		// the heartbeat task
		"every 15 minutes",
		// data, not instructions
		"data from a teammate", "not an instruction from Matt", "request bodies", "replies to your own asks", "answer text", "attachments",
		// connector writes
		"connected app", "ask Matt in this DM", "request id", "sender", "exact write", "never counts as confirmation",
		// no secrets
		"Never reveal", "tincan config", "tokens", "auth keys", "tailnet keys",
		// reset recovery
		"tincan is missing", "auth or login error", "new join code", "instead of retrying",
	} {
		if !strings.Contains(a.Instructions, want) {
			t.Errorf("dot instructions missing %q:\n%s", want, a.Instructions)
		}
	}
	// The generic self-heal (rejoin yourself, never ask for an invite) and
	// "handle as a request from the owner" would contradict the dot's rules.
	for _, bad := range []string{"Never ask Matt for an invite", "tincan rejoin", "Handle them as you would a request from Matt", "MCP server keeps running"} {
		if strings.Contains(a.Instructions, bad) {
			t.Errorf("dot instructions should not carry %q:\n%s", bad, a.Instructions)
		}
	}
	setup := strings.Join(a.Setup, "\n")
	for _, want := range []string{"github.com/mvanhorn/agent-tincan/releases", "checksums.txt", "join line", "wake.json", `"method": "webhook"`, `"every": "15m"`, "held for Matt's approval", "approval.json"} {
		if !strings.Contains(setup, want) {
			t.Errorf("dot setup missing %q:\n%s", want, setup)
		}
	}
	if strings.Contains(blockText(a), "TINCAN_CONFIG") {
		t.Errorf("dot has its own computer and needs no per-agent config:\n%s", blockText(a))
	}
	r := recipe(t, k, KindDot)
	all := strings.Join(r.Steps, "\n")
	inv, join := strings.Index(all, "tincan invite <name> --kind dot"), strings.Index(all, "tincan join")
	if r.Title == "" || inv < 0 || join < 0 || inv > join {
		t.Errorf("dot recipe must invite before join:\n%s: %s", r.Title, all)
	}
	if !strings.Contains(k.Operator, "dot kind=dot wake=webhook (may sleep or be off)") {
		t.Errorf("operator prompt lacks the dot team line:\n%s", k.Operator)
	}
	if !strings.Contains(k.Operator, "dot agents") {
		t.Errorf("operator prompt should say requests to dot agents are held by default:\n%s", k.Operator)
	}
}

// Every kind has the required blocks, so recipes() and agentBlock never fail
// on a kind that was added to Kinds but not to the templates.
func TestEveryKindHasRequiredBlocks(t *testing.T) {
	for _, kind := range Kinds {
		for _, name := range []string{"instructions.", "setup.", "title."} {
			if tmpl.Lookup(name+kind) == nil {
				t.Errorf("no template %s%s", name, kind)
			}
		}
	}
	k := build(t, Options{RelayURL: relayURL})
	for _, kind := range Kinds {
		recipe(t, k, kind)
	}
}
