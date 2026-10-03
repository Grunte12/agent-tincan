//go:build unix

package client

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Another process (here, another open file) holding the config lock makes
// a relay move wait to rewrite the file, so neither write is lost.
func TestUpdateSavedRelayWaitsForConfigLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	if err := SaveConfigTo(path, Config{Relay: "http://old:8787", Agent: "muse"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- updateSavedRelay(path, "http://old:8787", "http://new:8787") }()
	select {
	case err := <-done:
		t.Fatalf("rewrote the config while another writer held its lock (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The other writer saves its change and lets go.
	if err := SaveConfigTo(path, Config{Relay: "http://old:8787", Agent: "muse", RelayKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c := readConfig(t, path); c.Relay != "http://new:8787" || c.RelayKey != "k" {
		t.Fatalf("config %+v, want both writes kept", c)
	}
}
