package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// An agents table from before good-at lines gains the column on open. A line
// set on an agent survives a rejoin that rewrites its row and a reopen, an
// unknown name reports false, and "" clears it.
func TestAgentGoodAtMigratesAndSurvivesRejoin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE agents (name TEXT PRIMARY KEY, node_id TEXT NOT NULL, node_name TEXT NOT NULL, joined_at INTEGER NOT NULL, kind TEXT, node_user TEXT, last_seen_at INTEGER, version TEXT)`,
		`INSERT INTO agents VALUES ('hermes', 'nH', 'hermes-vm', 1790000000000, 'hermes', NULL, NULL, NULL)`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	s, c := open(t, path)
	ctx := context.Background()
	if a, _, err := s.AgentByName(ctx, "hermes"); err != nil || a.GoodAt != "" {
		t.Fatalf("after migration = %+v, %v", a, err)
	}
	if ok, err := s.SetAgentGoodAt(ctx, "hermes", "phone calls"); err != nil || !ok {
		t.Fatalf("set good-at: %v, %v", ok, err)
	}
	if ok, err := s.SetAgentGoodAt(ctx, "nobody", "phone calls"); err != nil || ok {
		t.Fatalf("unknown agent: %v, %v", ok, err)
	}
	// A rejoin rewrites the agent row; the line carries over like version.
	if err := s.PutAgent(ctx, identity.Agent{Name: "hermes", NodeID: "nH2", NodeName: "hermes-vm-2", JoinedAt: c.t, Kind: "hermes"}); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := s.AgentByName(ctx, "hermes"); a.GoodAt != "phone calls" || a.NodeID != "nH2" {
		t.Fatalf("after rejoin = %+v", a)
	}
	s.Close()
	s2, _ := open(t, path)
	if a, _, err := s2.AgentByName(ctx, "hermes"); err != nil || a.GoodAt != "phone calls" {
		t.Fatalf("after reopen = %+v, %v", a, err)
	}
	if ok, err := s2.SetAgentGoodAt(ctx, "hermes", ""); err != nil || !ok {
		t.Fatalf("clear: %v, %v", ok, err)
	}
	if a, _, _ := s2.AgentByName(ctx, "hermes"); a.GoodAt != "" {
		t.Fatalf("after clearing = %+v", a)
	}
}
