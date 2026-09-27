package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

func TestWebInstallWritesDefinitionWithoutLoading(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service definition on " + runtime.GOOS)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A PATH with no launchctl or systemctl proves install never runs them.
	t.Setenv("PATH", t.TempDir())
	for _, site := range []string{"chatgpt", "claude-ai"} {
		out, err := run(t, Root(), "web", "install", "--site", site, "--binary", "/opt/tincan/tincan")
		if err != nil {
			t.Fatalf("%s: %v\n%s", site, err, out)
		}
		agent := history.WebAgentName(history.Source(site))
		def := filepath.Join(home, ".config", "systemd", "user", "tincan-"+agent+".service")
		if runtime.GOOS == "darwin" {
			def = filepath.Join(home, "Library", "LaunchAgents", history.WebServiceLabel(history.Source(site))+".plist")
		}
		b, err := os.ReadFile(def)
		if err != nil {
			t.Fatalf("%s: definition not written: %v\n%s", site, err, out)
		}
		for _, want := range []string{"/opt/tincan/tincan", site, agent + ".json"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s: definition missing %q", site, want)
			}
		}
		for _, want := range []string{def, "(not started)", "TINCAN_CONFIG=~/.config/tincan/" + agent + ".json tincan join"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output missing %q:\n%s", site, want, out)
			}
		}
	}
	if _, err := run(t, Root(), "web", "install", "--site", "codex"); err == nil {
		t.Fatal("site codex accepted")
	}
	if _, err := run(t, Root(), "web", "install"); err == nil {
		t.Fatal("missing --site accepted")
	}
}

func TestWebServeRefusesAnotherAgentAndMissingConfig(t *testing.T) {
	t.Setenv("TINCAN_RELAY", "")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	_, err := run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", filepath.Join(t.TempDir(), "chatgpt-web.json"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "no relay configured") {
		t.Fatalf("no config: %v", err)
	}
	// Config joined as codex: refused before any poll or claim.
	srv := whoamiRelay(t, "codex")
	_, err = run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "codex"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "chatgpt-web") {
		t.Fatalf("codex config: %v", err)
	}
	// No agent in the config: the relay's answer decides.
	_, err = run(t, Root(), "web", "serve", "--site", "claude-ai", "--config", serveConfig(t, srv.URL, ""), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), `"codex"`) || !strings.Contains(err.Error(), "claude-web") {
		t.Fatalf("whoami codex: %v", err)
	}
	if err := os.WriteFile(allow, []byte("bad/name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("bad allowlist: %v", err)
	}
}

func TestHistoryInstallExtensionDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", filepath.Join(home, "native"))
	ext, err := filepath.Abs(filepath.Join("..", "..", "extension"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--no-service", "--extension-dir", ext)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	w, err := os.ReadFile(filepath.Join(home, "native", "native-host"))
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	if err != nil || !strings.Contains(string(w), history.ExtensionDirEnv+"='"+ext+"'") || !strings.Contains(out, "reloads itself") {
		t.Fatalf("wrapper:\n%s\noutput:\n%s", w, out)
	}
	if _, err := run(t, Root(), "history", "install", "--binary", "/opt/tincan/tincan", "--no-service", "--extension-dir", t.TempDir()); err == nil {
		t.Fatal("dir without a manifest accepted")
	}
}

// shortNativeDir is a native host directory short enough for a unix
// socket path, with HOME moved so nothing touches the real config.
func shortNativeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tcw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TINCAN_HISTORY_NATIVE_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

// fakeHostStatus serves the native host socket in dir and answers
// host.status with status, as a native host would.
func fakeHostStatus(t *testing.T, dir string, status history.ExtensionStatus) {
	t.Helper()
	ln, err := history.ListenSocket(filepath.Join(dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			b, err := history.ReadMessage(conn, 1<<20)
			var req history.NativeRequest
			if err == nil && json.Unmarshal(b, &req) == nil {
				resp := history.NativeResponse{ID: req.ID, Error: &history.NativeError{Code: "bad_request", Message: "unexpected op"}}
				if req.Op == history.OpHostStatus {
					res, _ := json.Marshal(status)
					resp = history.NativeResponse{ID: req.ID, OK: true, Result: res}
				}
				_ = history.WriteMessage(conn, resp, 1<<20)
			}
			conn.Close()
		}
	}()
}

// The extension is connected and says chatgpt.com is not granted: web
// serve exits at startup, naming the options page, before any poll
// (whoamiRelay fails the test on one).
func TestWebServeExitsWhenConnectedExtensionLacksTheGrant(t *testing.T) {
	dir := shortNativeDir(t)
	fakeHostStatus(t, dir, history.ExtensionStatus{Hello: true, Granted: []string{"claudeai"}})
	srv := whoamiRelay(t, "chatgpt-web")
	allow := filepath.Join(t.TempDir(), "allow.txt")
	_, err := run(t, Root(), "web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", allow)
	if err == nil || !strings.Contains(err.Error(), "no access to chatgpt.com") || !strings.Contains(err.Error(), "Extension options") {
		t.Fatalf("ungranted site: %v", err)
	}
}

// launchd starts web serve at login, often before Chrome: with no
// extension connected it starts, holds one long poll (no tight loop), and
// stops cleanly.
func TestWebServeWithoutExtensionStartsAndDoesNotLoop(t *testing.T) {
	shortNativeDir(t)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "chatgpt-web"})
		case "/v1/poll":
			polls.Add(1)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := Root()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"web", "serve", "--site", "chatgpt", "--config", serveConfig(t, srv.URL, "chatgpt-web"), "--allowlist", filepath.Join(t.TempDir(), "allow.txt")})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	time.Sleep(700 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("web serve exited without an extension: %v\n%s", err, out.String())
	default:
	}
	if n := polls.Load(); n != 1 {
		t.Fatalf("%d polls in 700ms; want one held long poll", n)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("web serve did not stop")
	}
}
