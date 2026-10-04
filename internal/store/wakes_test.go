package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// The last wake per agent is one row that each send replaces, and it
// survives a reopen, so a restarted relay still knows an agent it woke.
func TestLastWakeSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ws, err := s.LastWakes(ctx); err != nil || len(ws) != 0 {
		t.Fatalf("fresh store wakes = %v, %v", ws, err)
	}
	at := time.UnixMilli(1_790_000_000_000)
	if err := s.SetLastWake(ctx, "grokbot", Wake{At: at, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastWake(ctx, "grokbot", Wake{At: at.Add(time.Minute), Result: "hooks.example returned 502 Bad Gateway"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastWake(ctx, "instinct", Wake{At: at, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ws, err := s.LastWakes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := ws["grokbot"]; !g.At.Equal(at.Add(time.Minute)) || g.Result != "hooks.example returned 502 Bad Gateway" {
		t.Fatalf("grokbot = %+v", g)
	}
	if i := ws["instinct"]; !i.At.Equal(at) || i.Result != "ok" || len(ws) != 2 {
		t.Fatalf("wakes = %+v", ws)
	}
}

// An agent's last poll is its own column: only TouchAgentPoll moves it, only
// forward, it survives a reopen and a re-join, and it reads back per agent.
func TestAgentLastPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.UnixMilli(1_790_000_000_000)
	if err := s.PutAgent(ctx, identity.Agent{Name: "grokbot", NodeID: "nG", NodeName: "grok", JoinedAt: at}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.AgentLastPoll(ctx, "grokbot"); err != nil || !p.IsZero() {
		t.Fatalf("never polled = %v, %v", p, err)
	}
	if err := s.TouchAgent(ctx, "grokbot", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p, err := s.AgentLastPoll(ctx, "grokbot"); err != nil || !p.IsZero() {
		t.Fatalf("other activity moved the poll: %v, %v", p, err)
	}
	if err := s.TouchAgentPoll(ctx, "grokbot", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchAgentPoll(ctx, "grokbot", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgent(ctx, identity.Agent{Name: "grokbot", NodeID: "nG2", NodeName: "grok-2", JoinedAt: at.Add(3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, err := s.AgentLastPoll(ctx, "grokbot"); err != nil || !p.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("last poll = %v, %v", p, err)
	}
	if ps, err := s.AgentsLastPoll(ctx); err != nil || len(ps) != 1 || !ps["grokbot"].Equal(at.Add(2*time.Minute)) {
		t.Fatalf("all last polls = %v, %v", ps, err)
	}
	if p, err := s.AgentLastPoll(ctx, "nobody"); err != nil || !p.IsZero() {
		t.Fatalf("unknown agent = %v, %v", p, err)
	}
}

// A wake recorded late (a slow send finishing after a newer one) never
// replaces a newer wake.
func TestSetLastWakeKeepsNewer(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	t1 := time.UnixMilli(1_790_000_000_000)
	t2 := t1.Add(time.Minute)
	if err := s.SetLastWake(ctx, "grokbot", Wake{At: t2, Result: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastWake(ctx, "grokbot", Wake{At: t1, Result: "hooks.example returned 502 Bad Gateway"}); err != nil {
		t.Fatal(err)
	}
	if ws, err := s.LastWakes(ctx); err != nil || !ws["grokbot"].At.Equal(t2) || ws["grokbot"].Result != "ok" {
		t.Fatalf("wakes = %+v, %v", ws, err)
	}
}
