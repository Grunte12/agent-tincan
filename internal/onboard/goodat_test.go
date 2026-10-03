package onboard

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/identity"
)

// Every fixed-job kind has a stock good-at line and no other kind does, so a
// new service kind without one fails here.
func TestStockGoodAtCoversExactlyTheServiceKinds(t *testing.T) {
	var services, stocked []string
	for _, k := range Kinds {
		if isService(k) {
			services = append(services, k)
		}
		if StockGoodAt(k) != "" {
			stocked = append(stocked, k)
		}
	}
	var keys []string
	for k := range stockGoodAt {
		keys = append(keys, k)
	}
	slices.Sort(services)
	slices.Sort(stocked)
	slices.Sort(keys)
	if !slices.Equal(keys, services) || !slices.Equal(stocked, services) {
		t.Fatalf("stock lines for %v (map keys %v), want exactly the service kinds %v", stocked, keys, services)
	}
	if StockGoodAt("") != "" || StockGoodAt(KindCodex) != "" {
		t.Fatal("a general or unknown kind has a stock line")
	}
}

// Every stock line is one the relay would accept from the owner, stored
// unchanged, and follows the output hygiene rules.
func TestStockGoodAtLinesPassValidationAndHygiene(t *testing.T) {
	ctx := context.Background()
	store := identity.NewMemoryStore()
	dir := identity.NewDirectory(store, nil, identity.Config{})
	for kind, line := range stockGoodAt {
		if err := store.PutAgent(ctx, identity.Agent{Name: kind, Kind: kind}); err != nil {
			t.Fatal(err)
		}
		if err := dir.SetGoodAt(ctx, identity.LocalAdmin, kind, line); err != nil {
			t.Errorf("%s stock line refused: %v", kind, err)
			continue
		}
		if a, _, _ := store.AgentByName(ctx, kind); a.GoodAt != line {
			t.Errorf("%s stock line stored as %q, want %q unchanged", kind, a.GoodAt, line)
		}
		for _, bad := range []string{"—", "–", "**"} {
			if strings.Contains(line, bad) {
				t.Errorf("%s stock line contains forbidden %q: %q", kind, bad, line)
			}
		}
	}
}
