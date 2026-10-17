package client_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

const testPython = "/Library/Frameworks/Python.framework/Versions/3.12/bin/python3"

type sessionRouteRow struct {
	Agent     string `json:"agent"`
	Host      string `json:"host"`
	Project   string `json:"project"`
	SessionID string `json:"session_id"`
}

func sessionFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, err := os.MkdirTemp("/private/tmp", "tincan-router-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	registryDir := filepath.Join(root, "private")
	if err := os.Mkdir(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	return root, project, filepath.Join(registryDir, "routes.json")
}

func writeSessionRegistry(t *testing.T, path string, owners, sessions []sessionRouteRow) {
	t.Helper()
	state := struct {
		Version   int               `json:"version"`
		Owners    []sessionRouteRow `json:"owners"`
		Sessions  []sessionRouteRow `json:"sessions"`
		Preserved []any             `json:"preserved"`
	}{1, owners, sessions, []any{}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func scopedProjectClient(t *testing.T, m *testrelay.Mesh, agent string) *client.Relay {
	t.Helper()
	r, err := client.NewRelayForFile(client.Config{Relay: m.URL(agent), Agent: agent, SessionID: "00000000-0000-4000-8000-000000000099"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPrepareProjectUnifiedRouterAliasesUseExistingOwners(t *testing.T) {
	t.Setenv("TINCAN_SESSION_CATALOG", "local")
	m := testrelay.New(t, relay.Config{})
	aliases := []struct{ name, host, kind string }{
		{"native-codex", "codex", "codex"},
		{"native-cursor", "cursor", "cursor"},
		{"native-agy", "antigravity", "antigravity"},
		{"native-muse", "muse", "proxy-sandbox"},
		{"native-muse-generic", "muse", "generic"},
		{"native-muse-empty", "muse", ""},
	}
	for _, a := range aliases {
		m.JoinOnMachineOf(t, "muse", a.name)
		if ok, err := m.Store.SetAgentKind(t.Context(), a.name, a.kind); err != nil || !ok {
			t.Fatalf("set %s kind: ok=%v err=%v", a.name, ok, err)
		}
	}
	_, project, registry := sessionFixture(t)
	var owners, sessions []sessionRouteRow
	for i, a := range aliases {
		sid := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		row := sessionRouteRow{Agent: a.name, Host: a.host, Project: project, SessionID: sid}
		owners = append(owners, row)
		sessions = append(sessions, row)
		// A second discovered conversation must not replace the preserved owner.
		sessions = append(sessions, sessionRouteRow{Agent: a.name, Host: a.host, Project: project, SessionID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+101)})
	}
	writeSessionRegistry(t, registry, owners, sessions)
	hostMap, _ := json.Marshal(map[string]string{"native-codex": "codex", "native-cursor": "cursor", "native-agy": "antigravity", "native-muse": "muse", "native-muse-generic": "muse", "native-muse-empty": "muse"})
	t.Setenv("TINCAN_SESSION_HOSTS", string(hostMap))
	t.Setenv("TINCAN_SESSION_ROUTER", filepath.Join("..", "..", "examples", "session-routing", "router.py"))
	// The command runs from the package directory, so resolve the helper to an absolute path.
	helper, err := filepath.Abs(filepath.Join("..", "..", "examples", "session-routing", "router.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION_ROUTER", helper)
	t.Setenv("TINCAN_SESSION_REGISTRY", registry)
	t.Setenv("TINCAN_PROJECT_PYTHON", testPython)
	for i, a := range aliases {
		gotID, gotProject, err := scopedProjectClient(t, m, "grokbot").PrepareProject(t.Context(), a.name, project, fmt.Sprintf("model-%d", i))
		wantID := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if err != nil || gotID != wantID || gotProject != project {
			t.Fatalf("PrepareProject %s: id=%q project=%q err=%v", a.name, gotID, gotProject, err)
		}
	}
}

func TestPrepareProjectUnifiedRouterFailsClosedWithoutEnqueue(t *testing.T) {
	t.Setenv("TINCAN_SESSION_CATALOG", "local")
	m := testrelay.New(t, relay.Config{})
	m.JoinOnMachineOf(t, "muse", "native-codex")
	_, project, registry := sessionFixture(t)
	helper, err := filepath.Abs(filepath.Join("..", "..", "examples", "session-routing", "router.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION_ROUTER", helper)
	t.Setenv("TINCAN_PROJECT_PYTHON", testPython)
	t.Setenv("TINCAN_SESSION_REGISTRY", registry)
	t.Setenv("TINCAN_SESSION_HOSTS", `{"native-codex":"codex"}`)
	writeSessionRegistry(t, registry, nil, nil)
	r := scopedProjectClient(t, m, "grokbot")
	if _, _, err := r.PrepareProject(t.Context(), "native-codex", project, "model"); err == nil {
		t.Fatal("missing native owner unexpectedly resolved")
	}
	row1 := sessionRouteRow{Agent: "native-codex", Host: "codex", Project: project, SessionID: "00000000-0000-4000-8000-000000000011"}
	row2 := sessionRouteRow{Agent: "native-codex", Host: "codex", Project: project, SessionID: "00000000-0000-4000-8000-000000000012"}
	writeSessionRegistry(t, registry, nil, []sessionRouteRow{row1, row2})
	if _, _, err := r.PrepareProject(t.Context(), "native-codex", project, "model"); err == nil {
		t.Fatal("ambiguous native sessions unexpectedly resolved")
	}
	inbox, err := m.Client(t, "native-codex").Poll(t.Context(), 0)
	if err != nil || len(inbox.Requests) != 0 {
		t.Fatalf("failed preparation enqueued work: inbox=%+v err=%v", inbox, err)
	}
}

func TestPrepareProjectRequiresEnrolledRecipientAndValidHostMap(t *testing.T) {
	t.Setenv("TINCAN_SESSION_CATALOG", "local")
	m := testrelay.New(t, relay.Config{})
	_, project, registry := sessionFixture(t)
	helper, _ := filepath.Abs(filepath.Join("..", "..", "examples", "session-routing", "router.py"))
	t.Setenv("TINCAN_SESSION_ROUTER", helper)
	t.Setenv("TINCAN_SESSION_REGISTRY", registry)
	t.Setenv("TINCAN_PROJECT_PYTHON", testPython)
	t.Setenv("TINCAN_SESSION_HOSTS", `{bad json`)
	r := scopedProjectClient(t, m, "grokbot")
	if _, _, err := r.PrepareProject(t.Context(), "not-enrolled", project, "model"); err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("unenrolled recipient error = %v", err)
	}
	m.JoinOnMachineOf(t, "muse", "native-codex")
	if _, _, err := r.PrepareProject(t.Context(), "native-codex", project, "model"); err == nil || !strings.Contains(err.Error(), "invalid operator session host map") {
		t.Fatalf("invalid host map error = %v", err)
	}
}

func TestPrepareProjectKeepsConfiguredLegacyClaudeAdapter(t *testing.T) {
	t.Setenv("TINCAN_SESSION_CATALOG", "local")
	m := testrelay.New(t, relay.Config{})
	m.JoinOnMachineOf(t, "muse", "legacy-claude")
	_, project, registry := sessionFixture(t)
	root := filepath.Dir(registry)
	called := filepath.Join(root, "legacy-called")
	adapter := filepath.Join(root, "legacy-adapter.py")
	script := "import json, pathlib, sys\npathlib.Path(" + strconvQuote(called) + ").write_text('called')\nprint(json.dumps({'session_id':'00000000-0000-4000-8000-000000000021','project':sys.argv[2]}))\n"
	if err := os.WriteFile(adapter, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION_ROUTER", filepath.Join("..", "..", "examples", "session-routing", "router.py"))
	if helper, err := filepath.Abs(filepath.Join("..", "..", "examples", "session-routing", "router.py")); err == nil {
		t.Setenv("TINCAN_SESSION_ROUTER", helper)
	} else {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION_HOSTS", `{"legacy-claude":"claude-code"}`)
	t.Setenv("TINCAN_PROJECT_ADAPTER", adapter)
	t.Setenv("TINCAN_PROJECT_REGISTRY", filepath.Join(root, "legacy-registry.json"))
	t.Setenv("TINCAN_PROJECT_PYTHON", testPython)
	id, gotProject, err := scopedProjectClient(t, m, "grokbot").PrepareProject(t.Context(), "legacy-claude", project, "requested-model")
	if err != nil || id != "00000000-0000-4000-8000-000000000021" || gotProject != project {
		t.Fatalf("legacy PrepareProject: %q %q %v", id, gotProject, err)
	}
	if _, err := os.Stat(called); err != nil {
		t.Fatalf("legacy adapter was not selected: %v", err)
	}
}

func TestPrepareProjectUsesSharedNativeCatalogForAllHosts(t *testing.T) {
	t.Setenv("TINCAN_SESSION_CATALOG", "")
	t.Setenv("TINCAN_SESSION_ROUTER", "")
	aliases := []struct{ name, host, kind string }{
		{"native-codex", "codex", "codex"},
		{"native-cursor", "cursor", "cursor"},
		{"native-agy", "antigravity", "antigravity"},
		{"native-muse", "muse", "proxy-sandbox"},
		{"native-claude", "claude-code", "claude-code"},
	}
	hosts := make(map[string]string, len(aliases))
	for _, a := range aliases {
		hosts[a.name] = a.host
	}
	hostMap, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_SESSION_HOSTS", string(hostMap))
	m := testrelay.New(t, relay.Config{})
	origin := scopedProjectClient(t, m, "grokbot")
	for i, a := range aliases {
		m.JoinOnMachineOf(t, "muse", a.name)
		if ok, err := m.Store.SetAgentKind(t.Context(), a.name, a.kind); err != nil || !ok {
			t.Fatalf("set %s kind: ok=%v err=%v", a.name, ok, err)
		}
		project := fmt.Sprintf("/synthetic/workspaces/project-%d", i)
		firstSession := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+201)
		lateSession := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+301)
		native, err := client.NewRelayForFile(client.Config{Relay: m.URL(a.name), Agent: a.name, SessionID: firstSession}, "")
		if err != nil {
			t.Fatal(err)
		}
		binding, err := native.DiscoverSession(t.Context(), a.host, project)
		if err != nil || binding.SessionID != firstSession || binding.Project != project {
			t.Fatalf("discover %s binding=%+v err=%v", a.name, binding, err)
		}
		gotID, gotProject, err := origin.PrepareProject(t.Context(), a.name, project, "requested-model-is-ignored")
		if err != nil || gotID != firstSession || gotProject != project {
			t.Fatalf("shared resolve %s id=%q project=%q err=%v", a.name, gotID, gotProject, err)
		}
		late, err := client.NewRelayForFile(client.Config{Relay: m.URL(a.name), Agent: a.name, SessionID: lateSession}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := late.DiscoverSession(t.Context(), a.host, project); err != nil {
			t.Fatalf("late discovery %s: %v", a.name, err)
		}
		gotID, gotProject, err = origin.PrepareProject(t.Context(), a.name, project, "another-model")
		if err != nil || gotID != firstSession || gotProject != project {
			t.Fatalf("late chat changed immutable owner for %s: id=%q project=%q err=%v", a.name, gotID, gotProject, err)
		}
		if _, err := os.Stat(project); !os.IsNotExist(err) {
			t.Fatalf("catalog path unexpectedly touched local workspace %q: %v", project, err)
		}
		inbox, err := m.Client(t, a.name).Poll(t.Context(), 0)
		if err != nil || len(inbox.Requests) != 0 {
			t.Fatalf("project resolution created relay work for %s: inbox=%+v err=%v", a.name, inbox, err)
		}
	}
}

func strconvQuote(value string) string { b, _ := json.Marshal(value); return string(b) }
