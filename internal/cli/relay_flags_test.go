package cli

import (
	"strings"
	"testing"
	"time"
)

// --notes-ttl defaults to 30 days, reaches the relay config, and must be
// positive.
func TestRelayNotesTTLFlag(t *testing.T) {
	cmd := relayCmd()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if d, err := cmd.Flags().GetDuration("notes-ttl"); err != nil || d != 30*24*time.Hour {
		t.Fatalf("default --notes-ttl = %v %v, want 720h", d, err)
	}
	if got := (relayFlags{notesTTL: 72 * time.Hour}).relayConfig().NotesRequestTTL; got != 72*time.Hour {
		t.Fatalf("relay config notes ttl = %v, want 72h", got)
	}
	bad := relayCmd()
	if err := bad.Flags().Parse([]string{"--notes-ttl", "-1h"}); err != nil {
		t.Fatal(err)
	}
	if err := bad.RunE(bad, nil); err == nil || !strings.Contains(err.Error(), "--notes-ttl") {
		t.Fatalf("negative --notes-ttl: %v", err)
	}
}
