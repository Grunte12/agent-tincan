package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	catalogOriginSession = "00000000-0000-4000-8000-000000000031"
	catalogTargetSession = "00000000-0000-4000-8000-000000000032"
)

func sessionCatalogConfig(t *testing.T, relayURL, agent string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.json")
	cfg := client.Config{Relay: relayURL, Agent: agent, SessionID: "saved-placeholder", RelayKey: "fixture-relay-key", RelayInfoAt: time.Now()}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_CONFIG", path)
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
}

func executeSessionCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := Root()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil && stderr.Len() != 0 {
		err = fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), err
}

func TestSessionDiscoverUsesNativeRuntimeScopeWithoutIdentityFlags(t *testing.T) {
	var capabilityCalls, discoveryCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			capabilityCalls++
			_, _ = io.WriteString(w, `{"session_routing":true,"session_catalog":true}`)
			return
		}
		discoveryCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/session-discovery" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get(client.AgentHeader); got != "native-codex" {
			t.Errorf("agent header %q", got)
		}
		if got := r.Header.Get(client.SessionHeader); got != catalogOriginSession {
			t.Errorf("session header %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if len(body) != 2 || string(body["host"]) != `"codex"` || string(body["project"]) != `"/synthetic/project"` {
			t.Errorf("discovery body can override identity or has wrong scope: %s", mustJSON(body))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": envelope.SessionBinding{Agent: "native-codex", Host: "codex", Project: "/synthetic/project", SessionID: catalogOriginSession}, "changed": true})
	}))
	defer server.Close()
	sessionCatalogConfig(t, server.URL, "native-codex")
	t.Setenv("TINCAN_SESSION_ID", catalogOriginSession)
	t.Setenv("CODEX_THREAD_ID", "inherited-codex-placeholder")
	out, err := executeSessionCLI(t, "session-discover", "--host", "codex", "--project", "/synthetic/project")
	if err != nil {
		t.Fatal(err)
	}
	var got envelope.SessionBinding
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid binding output %q: %v", out, err)
	}
	if got.Agent != "native-codex" || got.SessionID != catalogOriginSession || got.Host != "codex" || got.Project != "/synthetic/project" {
		t.Fatalf("binding = %+v", got)
	}
	if capabilityCalls != 1 || discoveryCalls != 1 {
		t.Fatalf("capability calls=%d discovery calls=%d", capabilityCalls, discoveryCalls)
	}
	if _, err := executeSessionCLI(t, "session-discover", "--host", "codex", "--project", "/synthetic/project", "--session-id", catalogTargetSession); err == nil {
		t.Fatal("session-discover accepted a manual session ID override")
	}
	if _, err := executeSessionCLI(t, "session-discover", "--host", "codex", "--project", "/synthetic/project", "--agent", "other-agent"); err == nil {
		t.Fatal("session-discover accepted an agent identity override")
	}
}

func TestSessionDiscoverRequiresRuntimeIDEvenWhenConfigHasPlaceholder(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"session_routing":true,"session_catalog":true}`)
	}))
	defer server.Close()
	sessionCatalogConfig(t, server.URL, "native-codex")
	t.Setenv("TINCAN_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "inherited-codex-placeholder")
	if _, err := executeSessionCLI(t, "session-discover", "--host", "codex", "--project", "/synthetic/project"); err == nil || !strings.Contains(err.Error(), "native runtime session ID required") {
		t.Fatalf("missing runtime session error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("missing runtime scope made %d HTTP calls", calls)
	}
}

func TestProjectSessionUsesAuthenticatedOriginAndAgentArgumentOnlyForTarget(t *testing.T) {
	var capabilityCalls, resolveCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			capabilityCalls++
			_, _ = io.WriteString(w, `{"session_routing":true,"session_catalog":true}`)
			return
		}
		resolveCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/project-session" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if len(q) != 3 || q.Get("agent") != "recipient" || q.Get("host") != "cursor" || q.Get("project") != "/synthetic/project" {
			t.Errorf("resolve query = %v", q)
		}
		if r.Header.Get(client.AgentHeader) != "origin" || r.Header.Get(client.SessionHeader) != catalogOriginSession {
			t.Errorf("origin headers agent=%q session=%q", r.Header.Get(client.AgentHeader), r.Header.Get(client.SessionHeader))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": envelope.SessionBinding{Agent: "recipient", Host: "cursor", Project: "/synthetic/project", SessionID: catalogTargetSession}, "changed": true})
	}))
	defer server.Close()
	sessionCatalogConfig(t, server.URL, "origin")
	t.Setenv("TINCAN_SESSION_ID", catalogOriginSession)
	t.Setenv("CODEX_THREAD_ID", "untrusted-origin-placeholder")
	out, err := executeSessionCLI(t, "project-session", "recipient", "--host", "cursor", "--project", "/synthetic/project")
	if err != nil {
		t.Fatal(err)
	}
	var got envelope.SessionBinding
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.SessionID != catalogTargetSession {
		t.Fatalf("binding=%+v decode err=%v output=%q", got, err, out)
	}
	if capabilityCalls != 1 || resolveCalls != 1 {
		t.Fatalf("capability calls=%d resolve calls=%d", capabilityCalls, resolveCalls)
	}
}

func TestSessionPreservePostsTypedBindingWithoutTouchingRequests(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/admin/session-owners/preserve" {
			t.Errorf("unexpected admin request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		var binding envelope.SessionBinding
		if err := json.NewDecoder(r.Body).Decode(&binding); err != nil {
			t.Errorf("decode typed session binding: %v", err)
		}
		if binding != (envelope.SessionBinding{Agent: "recipient", Host: "muse", SessionID: catalogTargetSession, Project: ""}) {
			t.Errorf("preservation body = %+v", binding)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": binding, "changed": true})
	}))
	defer server.Close()
	sessionCatalogConfig(t, server.URL, "")
	t.Setenv("TINCAN_SESSION_ID", "")
	out, err := executeSessionCLI(t, "session-preserve", "--relay", server.URL, "--agent", "recipient", "--host", "muse", "--session-id", catalogTargetSession)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"changed": true`) || !strings.Contains(out, catalogTargetSession) {
		t.Fatalf("preserve output = %s", out)
	}
	if calls != 1 {
		t.Fatalf("preserve made %d requests; it must not alter, cancel, or resend old work", calls)
	}
}

func TestSessionCatalogCommandsRegisteredWithOnlyDocumentedIdentityInputs(t *testing.T) {
	root := Root()
	for _, name := range []string{"session-discover", "project-session", "session-preserve"} {
		if root.Commands()[0] == nil { // force root construction before looking up commands
			t.Fatal("empty command tree")
		}
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Errorf("%s not registered: %v", name, err)
		}
	}
	if sessionDiscoverCmd().Flags().Lookup("agent") != nil || sessionDiscoverCmd().Flags().Lookup("session-id") != nil {
		t.Fatal("discovery command exposes identity override flags")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
