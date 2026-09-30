package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

func approvalFile(t *testing.T, body string) *Approval {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := LoadApproval(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func prepare(t *testing.T, f *fixture, from, to, parent string, kind envelope.Kind) envelope.Request {
	t.Helper()
	req := envelope.Request{From: from, To: to, Kind: kind, Body: "x", ParentID: parent}
	if err := f.pol.Prepare(t.Context(), &req); err != nil {
		t.Fatalf("%s -> %s: %v", from, to, err)
	}
	return req
}

// Requests to a dot are held for the owner with no config at all: the
// default TTL and no notify agent, exactly what a gated target gets from an
// approval.json without hold_ttl or notify.
func TestDotHeldByDefaultWithoutConfig(t *testing.T) {
	for _, a := range []*Approval{nil, approvalFile(t, "")} {
		f := newFixture(t, Config{Approval: a})
		f.agent(t, "dot-web", onboard.KindDotWeb)
		for _, kind := range []envelope.Kind{envelope.KindAsk, envelope.KindNotify} {
			req := prepare(t, f, "muse", "dot-web", "", kind)
			if req.Status != envelope.StatusHeld || req.HoldTTL != 2*time.Hour || req.ApprovalNotify != "" {
				t.Fatalf("%s to dot (approval %v) = status %q ttl %v notify %q", kind, a != nil, req.Status, req.HoldTTL, req.ApprovalNotify)
			}
		}
		ping := prepare(t, f, "muse", "dot-web", "", envelope.KindPing)
		if ping.Status == envelope.StatusHeld {
			t.Fatal("a ping to a dot was held")
		}
	}
}

// With an approval.json that gates other targets, a dot's default hold uses
// its hold_ttl and notify, the same as a gated target.
func TestDotDefaultHoldUsesPolicyTTLAndNotify(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"muse":{"from":"*"}},"hold_ttl":"3h","notify":"instinct"}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	req := prepare(t, f, "grokbot", "dot-web", "", envelope.KindAsk)
	if req.Status != envelope.StatusHeld || req.HoldTTL != 3*time.Hour || req.ApprovalNotify != "instinct" {
		t.Fatalf("dot hold = %q %v %q", req.Status, req.HoldTTL, req.ApprovalNotify)
	}
}

// An approval.json entry for the dot replaces the default: a sender it
// allows goes straight through, one it gates is held.
func TestDotApprovalEntryOverridesDefault(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"dot-web":{"unless":["muse"]}}}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	if req := prepare(t, f, "muse", "dot-web", "", envelope.KindAsk); req.Status == envelope.StatusHeld {
		t.Fatal("an allowed sender was held")
	}
	if req := prepare(t, f, "grokbot", "dot-web", "", envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("a sender the entry gates was not held")
	}
	// An entry that gates nobody also opens the dot to everyone.
	g := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"dot-web":{"from":[]}}}`)})
	g.agent(t, "dot-web", onboard.KindDotWeb)
	if req := prepare(t, g, "grokbot", "dot-web", "", envelope.KindAsk); req.Status == envelope.StatusHeld {
		t.Fatal("an empty from entry did not open the dot")
	}
}

// Other kinds, and agents with no kind, are not held without an entry. That
// includes every other web agent: only dot-web fronts an agent that acts in
// the owner's apps.
func TestOtherKindsNotHeldByDefault(t *testing.T) {
	for _, a := range []*Approval{nil, approvalFile(t, "")} {
		f := newFixture(t, Config{Approval: a})
		f.agent(t, "cx", onboard.KindCodex)
		f.agent(t, "fo", onboard.KindScheduled)
		targets := []string{"cx", "fo", "muse"}
		for _, kind := range []string{onboard.KindChatGPTWeb, onboard.KindClaudeWeb, onboard.KindGrokWeb, onboard.KindGeminiWeb, onboard.KindPerplexityWeb, onboard.KindCopilotWeb} {
			f.agent(t, kind, kind)
			targets = append(targets, kind)
		}
		for _, to := range targets {
			if req := prepare(t, f, "grokbot", to, "", envelope.KindAsk); req.Status == envelope.StatusHeld {
				t.Errorf("request to %s held", to)
			}
		}
	}
}

// The dot's own asks are not held by default. An owner who wants them held
// gates the targets with from: ["dot-web"], which matches anywhere in the chain.
func TestGateFromDotHoldsAnywhereInChain(t *testing.T) {
	f := newFixture(t, Config{Approval: approvalFile(t, `{"gate":{"cx":{"from":["dot-web"]}}}`)})
	f.agent(t, "dot-web", onboard.KindDotWeb)
	f.agent(t, "cx", onboard.KindCodex)
	own := prepare(t, f, "dot-web", "muse", "", envelope.KindAsk)
	if own.Status == envelope.StatusHeld {
		t.Fatal("the dot's own ask to an ungated agent was held")
	}
	if req := prepare(t, f, "dot-web", "cx", "", envelope.KindAsk); req.Status != envelope.StatusHeld {
		t.Fatal("dot -> cx not held")
	}
	first, err := f.st.Enqueue(t.Context(), own, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.claim(t, first.ID, "muse")
	req := prepare(t, f, "muse", "cx", first.ID, envelope.KindAsk)
	if req.Status != envelope.StatusHeld || len(req.Chain) != 2 || req.Chain[0] != "dot-web" {
		t.Fatalf("dot -> muse -> cx = %q %v", req.Status, req.Chain)
	}
}
