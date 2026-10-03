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

// A writer that holds the config lock and never lets go cannot stall a
// relay move: taking the lock gives up after configLockWait, and the
// moved address stays applied in memory.
func TestUpdateSavedRelayGivesUpOnAStuckLock(t *testing.T) {
	old := configLockWait
	configLockWait = 150 * time.Millisecond
	t.Cleanup(func() { configLockWait = old })
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
		if err == nil {
			t.Fatal("rewrote the config while another writer held its lock")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still waiting on a lock held by a stuck writer")
	}
	if c := readConfig(t, path); c.Relay != "http://old:8787" {
		t.Fatalf("config %+v changed without the lock", c)
	}
}

// A move whose save met a held config lock saves once the lock is free,
// under the same rule as the first try.
func TestRelayMoveSaveRetriesAfterTheLockFrees(t *testing.T) {
	oldWait, oldEvery := configLockWait, saveRetryEvery
	configLockWait, saveRetryEvery = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { configLockWait, saveRetryEvery = oldWait, oldEvery })
	path := filepath.Join(t.TempDir(), "client.json")
	if err := SaveConfigTo(path, Config{Relay: "http://old:8787", Agent: "muse"}); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRelayForFile(Config{Relay: "http://new:8787", Agent: "muse"}, path)
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.retrySave("http://old:8787", "http://new:8787"); close(done) }()
	time.Sleep(120 * time.Millisecond)
	if c := readConfig(t, path); c.Relay != "http://old:8787" {
		t.Fatalf("saved %+v while the lock was held", c)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not finish after the lock freed")
	}
	if c := readConfig(t, path); c.Relay != "http://new:8787" {
		t.Fatalf("config %+v, want the move saved", c)
	}
}

// A retry gives up once the client has moved again, so it never saves a
// stale address.
func TestRelayMoveSaveRetryStopsAfterAnotherMove(t *testing.T) {
	oldEvery := saveRetryEvery
	saveRetryEvery = 10 * time.Millisecond
	t.Cleanup(func() { saveRetryEvery = oldEvery })
	path := filepath.Join(t.TempDir(), "client.json")
	if err := SaveConfigTo(path, Config{Relay: "http://old:8787", Agent: "muse"}); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRelayForFile(Config{Relay: "http://newer:8787", Agent: "muse"}, path)
	r.retrySave("http://old:8787", "http://new:8787")
	if c := readConfig(t, path); c.Relay != "http://old:8787" {
		t.Fatalf("config %+v, want it left alone after another move", c)
	}
}

// A retry that finds the file changed by another writer leaves it alone.
func TestRelayMoveSaveRetryLeavesAChangedFile(t *testing.T) {
	oldEvery := saveRetryEvery
	saveRetryEvery = 10 * time.Millisecond
	t.Cleanup(func() { saveRetryEvery = oldEvery })
	path := filepath.Join(t.TempDir(), "client.json")
	if err := SaveConfigTo(path, Config{Relay: "http://chosen:8787", Agent: "muse"}); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRelayForFile(Config{Relay: "http://new:8787", Agent: "muse"}, path)
	r.retrySave("http://old:8787", "http://new:8787")
	if c := readConfig(t, path); c.Relay != "http://chosen:8787" {
		t.Fatalf("config %+v, want the other writer's relay kept", c)
	}
}
