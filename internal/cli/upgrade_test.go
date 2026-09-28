package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

var platformFile = "tincan_" + runtime.GOOS + "_" + runtime.GOARCH

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeExe writes a stand-in for the running tincan binary and returns its
// path and file info.
func fakeExe(t *testing.T) (string, os.FileInfo) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tincan")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, fi
}

func meshWithDist(t *testing.T, files map[string]string) *testrelay.Mesh {
	t.Helper()
	m := testrelay.New(t, relay.Config{})
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.Server.SetDist(dir)
	return m
}

func assertUntouched(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "old binary" {
		t.Fatalf("executable = %q, %v; want the old binary untouched", raw, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("executable was replaced")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files next to the executable: %v", entries)
	}
}

// An agent without GitHub access updates from the relay: the new binary goes
// to a new inode renamed over the old path, never written in place.
func TestUpgradeReplacesExecutable(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil || string(raw) != "new binary" {
		t.Fatalf("executable = %q, %v", raw, err)
	}
	after, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("binary was overwritten in place; want a new inode renamed over it")
	}
	if after.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", after.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
	for _, want := range []string{"0.4.0", "Restart any long-running tincan processes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

// An executable that already matches the relay's build is left alone.
func TestUpgradeAlreadyCurrent(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "old binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUpgradeCheckDoesNotWrite(t *testing.T) {
	m := meshWithDist(t, map[string]string{platformFile: "new binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	var out bytes.Buffer
	if err := upgrade(t.Context(), m.Client(t, "muse"), exe, true, &out); err != nil {
		t.Fatal(err)
	}
	assertUntouched(t, exe, before)
	for _, want := range []string{Version, "0.4.0", "tincan upgrade"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check output missing %q:\n%s", want, out.String())
		}
	}
}

func TestUpgradeRefusesMissingPlatform(t *testing.T) {
	other := "tincan_linux_arm64"
	if platformFile == other {
		other = "tincan_darwin_arm64"
	}
	m := meshWithDist(t, map[string]string{other: "someone else's binary", "VERSION": "0.4.0"})
	exe, before := fakeExe(t)
	err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), platformFile) {
		t.Fatalf("err = %v, want one naming %s", err, platformFile)
	}
	assertUntouched(t, exe, before)
}

// A download that does not match the manifest checksum never replaces the
// binary.
func TestUpgradeRefusesChecksumMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dist":
			json.NewEncoder(w).Encode(client.DistManifest{Version: "0.4.0", Files: []client.DistFile{{Name: platformFile, SHA256: sha([]byte("the real build"))}}})
		case "/v1/dist/" + platformFile:
			w.Write([]byte("tampered build"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	r, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	exe, before := fakeExe(t)
	err = upgrade(t.Context(), r, exe, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	assertUntouched(t, exe, before)
}

// A relay without --dist says so instead of failing obscurely.
func TestUpgradeWithoutDist(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	exe, before := fakeExe(t)
	err := upgrade(t.Context(), m.Client(t, "muse"), exe, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--dist") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, exe, before)
}

// After an upgrade, tincan names each tincan mcp still on the old build and
// how its app reloads it; with none running it lists the step per app.
func TestReloadAdvice(t *testing.T) {
	alive := func(mcpserver.Launch) bool { return true }
	ls := []mcpserver.Launch{
		{PID: 10, Client: "claude-code 2.0.1", Version: "0.3.0"},
		{PID: 11, Client: "cursor-vscode 1", Version: "0.4.0"},
		{PID: 12, Client: "codex-mcp-client 1", Version: "0.3.0", Ended: time.Now()},
	}
	got := reloadAdvice(ls, "0.4.0", alive)
	for _, want := range []string{"pid 10", "Claude Code", "Restart any long-running tincan processes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("advice lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"pid 11", "pid 12"} {
		if strings.Contains(got, not) {
			t.Fatalf("advice names %s:\n%s", not, got)
		}
	}
	none := reloadAdvice(nil, "0.4.0", alive)
	for _, want := range []string{"Claude Code", "Codex", "Cursor", "other apps"} {
		if !strings.Contains(none, want) {
			t.Fatalf("advice with no launches lacks %q:\n%s", want, none)
		}
	}
}
