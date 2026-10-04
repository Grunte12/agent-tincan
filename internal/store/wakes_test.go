package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
