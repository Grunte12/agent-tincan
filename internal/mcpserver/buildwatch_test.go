package mcpserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mvanhorn/agent-tincan/internal/mcpserver"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// fakeBinary is a stand-in tincan binary whose version the watch reads
// through a counted fake instead of running it.
type fakeBinary struct {
	path    string
	version atomic.Value // string
	execs   atomic.Int32
	now     atomic.Int64 // unix nanoseconds of the fake clock
}

func (b *fakeBinary) advance(d time.Duration) { b.now.Add(int64(d)) }

func newFakeBinary(t *testing.T, version string) *fakeBinary {
	t.Helper()
	b := &fakeBinary{path: filepath.Join(t.TempDir(), "tincan")}
	b.now.Store(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).UnixNano())
	b.replace(t, version)
	return b
}

// replace writes a new file over the path, as tincan upgrade does, with
// content of a different size so the change shows even on coarse mtimes.
func (b *fakeBinary) replace(t *testing.T, version string) {
	t.Helper()
	tmp := b.path + ".new"
	if err := os.WriteFile(tmp, []byte("tincan build "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		t.Fatal(err)
	}
	b.version.Store(version)
}

func (b *fakeBinary) watch(running string) *mcpserver.BuildWatch {
	w := mcpserver.NewBuildWatch(b.path, running)
	w.Now = func() time.Time { return time.Unix(0, b.now.Load()) }
	w.ReadVersion = func(context.Context, string) (string, error) {
		b.execs.Add(1)
		return b.version.Load().(string), nil
	}
	return w
}

// watchedSession connects a client named clientName to a server that
// watches w.
func watchedSession(t *testing.T, m *testrelay.Mesh, w *mcpserver.BuildWatch, clientName string) *mcp.ClientSession {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	srv := mcpserver.New(m.Client(t, "grokbot"), "0.6.0", mcpserver.Watch(w))
	ss, err := srv.Connect(t.Context(), srvT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.0"}, nil).Connect(t.Context(), cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

const staleMarker = "keep running tincan 0.6.0"

// After tincan upgrade swaps the binary, the next tool call (at most a
// minute later) says so once, with the reload step for the host; later
// calls stay quiet until yet another build lands.
func TestStaleBuildNoticeOncePerVersion(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	b := newFakeBinary(t, "0.6.0")
	cs := watchedSession(t, m, b.watch("0.6.0"), "claude-code")

	if out := call(t, cs, "list_agents", nil); strings.Contains(out, staleMarker) {
		t.Fatalf("notice before any upgrade: %q", out)
	}
	b.replace(t, "0.7.0")
	// Within the minute the file is not looked at again.
	b.advance(30 * time.Second)
	if out := call(t, cs, "list_agents", nil); strings.Contains(out, staleMarker) {
		t.Fatalf("looked at the binary again within a minute: %q", out)
	}
	b.advance(time.Minute)
	out := call(t, cs, "check_inbox", nil)
	for _, want := range []string{staleMarker, "tincan 0.7.0", b.path, "Claude Code"} {
		if !strings.Contains(out, want) {
			t.Fatalf("check_inbox lacks %q: %q", want, out)
		}
	}
	b.advance(2 * time.Minute)
	if out := call(t, cs, "list_agents", nil); strings.Contains(out, staleMarker) {
		t.Fatalf("notice repeated for the same version: %q", out)
	}
	if n := b.execs.Load(); n != 1 {
		t.Fatalf("ran the binary %d times for one change, want 1", n)
	}
	b.replace(t, "0.7.1")
	b.advance(2 * time.Minute)
	if out := call(t, cs, "list_agents", nil); !strings.Contains(out, "tincan 0.7.1") {
		t.Fatalf("no notice for the next version: %q", out)
	}
}

// An unchanged binary costs a stat, never an exec, and adds nothing.
func TestUnchangedBinaryNoNoticeNoExec(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	b := newFakeBinary(t, "0.6.0")
	cs := watchedSession(t, m, b.watch("0.6.0"), "claude-code")
	for range 3 {
		b.advance(2 * time.Minute)
		if out := call(t, cs, "list_agents", nil); strings.Contains(out, staleMarker) {
			t.Fatalf("notice for an unchanged binary: %q", out)
		}
	}
	if n := b.execs.Load(); n != 0 {
		t.Fatalf("ran the binary %d times, want 0", n)
	}
}

// A binary rebuilt as the same version is not a stale server.
func TestSameVersionRebuildNoNotice(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	b := newFakeBinary(t, "0.6.0")
	cs := watchedSession(t, m, b.watch("0.6.0"), "codex-mcp-client")
	if err := os.WriteFile(b.path, []byte("rebuilt, longer, same version 0.6.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.advance(2 * time.Minute)
	if out := call(t, cs, "list_agents", nil); strings.Contains(out, staleMarker) {
		t.Fatalf("notice for a same-version rebuild: %q", out)
	}
}

// Each host gets its own reload step.
func TestReloadStepPerKind(t *testing.T) {
	cases := map[string]string{
		"claude-code":      mcpserver.HostClaudeCode,
		"codex-mcp-client": mcpserver.HostCodex,
		"cursor-vscode":    mcpserver.HostCursor,
		"claude-ai":        mcpserver.HostGeneric,
		"":                 mcpserver.HostGeneric,
	}
	for client, want := range cases {
		if got := mcpserver.HostKind(client); got != want {
			t.Errorf("HostKind(%q) = %q, want %q", client, got, want)
		}
	}
	seen := map[string]string{}
	for _, kind := range []string{mcpserver.HostClaudeCode, mcpserver.HostCodex, mcpserver.HostCursor, mcpserver.HostGeneric} {
		step := mcpserver.ReloadStep(kind)
		if step == "" {
			t.Fatalf("no reload step for %s", kind)
		}
		if other, dup := seen[step]; dup {
			t.Fatalf("%s and %s share the reload step %q", kind, other, step)
		}
		seen[step] = kind
	}
	for kind, want := range map[string]string{mcpserver.HostClaudeCode: "Claude Code", mcpserver.HostCodex: "Codex", mcpserver.HostCursor: "Cursor", mcpserver.HostGeneric: "app"} {
		if !strings.Contains(mcpserver.ReloadStep(kind), want) {
			t.Errorf("ReloadStep(%s) = %q, want it to name %q", kind, mcpserver.ReloadStep(kind), want)
		}
	}

	m := testrelay.New(t, relay.Config{})
	for client, want := range map[string]string{"codex-mcp-client": "Codex", "cursor-vscode": "Cursor"} {
		b := newFakeBinary(t, "0.6.0")
		cs := watchedSession(t, m, b.watch("0.6.0"), client)
		call(t, cs, "list_agents", nil)
		b.replace(t, "0.7.0")
		b.advance(2 * time.Minute)
		if out := call(t, cs, "list_agents", nil); !strings.Contains(out, want) {
			t.Fatalf("%s notice does not name %s: %q", client, want, out)
		}
	}
}
