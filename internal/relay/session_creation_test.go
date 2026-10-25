package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetOrCreateProjectSessionIsolation(t *testing.T) {
	var creates atomic.Int32
	h := newHarness(t, Config{CreateProjectSession: func(ctx context.Context, agent, host, project string) (envelope.SessionBinding, error) {
		n := creates.Add(1)
		return envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: fmt.Sprintf("native-%d", n)}, nil
	}})
	// An existing owner must never invoke the provisioner.
	h.catalogDo(museAddr, "muse", "existing-native", "POST", "/v1/session-discovery", `{"host":"antigravity","project":"/existing"}`, 200, nil)
	ensure := func(project string) envelope.SessionBinding {
		raw, _ := json.Marshal(map[string]string{"agent": "muse", "host": "antigravity", "project": project})
		r := httptest.NewRequest("POST", "/v1/project-session", strings.NewReader(string(raw)))
		r.RemoteAddr = grokAddr
		r.Header.Set(client.AgentHeader, "grokbot")
		r.Header.Set(client.SessionHeader, "origin")
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("status %d", w.Code)
			return envelope.SessionBinding{}
		}
		var out struct {
			Binding envelope.SessionBinding `json:"binding"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.Binding
	}
	if got := ensure("/existing"); got.SessionID != "existing-native" || creates.Load() != 0 {
		t.Fatalf("existing owner not reused: %+v", got)
	}
	var wg sync.WaitGroup
	results := make(chan envelope.SessionBinding, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results <- ensure(fmt.Sprintf("/project-%d", i%2)) }(i)
	}
	wg.Wait()
	close(results)
	ids := map[string]string{}
	for b := range results {
		if old := ids[b.Project]; old != "" && old != b.SessionID {
			t.Error("duplicate project owner")
		}
		ids[b.Project] = b.SessionID
	}
	if creates.Load() != 2 || len(ids) != 2 || ids["/project-0"] == ids["/project-1"] {
		t.Fatalf("crossed or duplicated routes: creates=%d ids=%v", creates.Load(), ids)
	}
}

func TestGetOrCreateRecoversThroughIdempotentProvisioner(t *testing.T) {
	var calls, creates atomic.Int32
	h := newHarness(t, Config{CreateProjectSession: func(context.Context, string, string, string) (envelope.SessionBinding, error) {
		if calls.Add(1) == 1 {
			creates.Add(1) // The provisioner's durable journal prevents another native attempt.
		}
		return envelope.SessionBinding{}, errors.New("uncertain native result")
	}})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/v1/project-session", strings.NewReader(`{"agent":"muse","host":"antigravity","project":"/new"}`))
		r.RemoteAddr = grokAddr
		r.Header.Set(client.AgentHeader, "grokbot")
		r.Header.Set(client.SessionHeader, "origin")
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatal("uncertain creation accepted")
		}
	}
	if creates.Load() != 1 || calls.Load() != 2 {
		t.Fatal("uncertain creation retried")
	}
}

func TestGetOrCreateAllLocalHostsRecoverAfterPreflightFailure(t *testing.T) {
	for _, host := range []string{"claude-code", "codex", "cursor", "antigravity", "opencode", "hermes"} {
		t.Run(host, func(t *testing.T) {
			var calls atomic.Int32
			h := newHarness(t, Config{CreateProjectSession: func(ctx context.Context, agent, nativeHost, project string) (envelope.SessionBinding, error) {
				if calls.Add(1) == 1 {
					return envelope.SessionBinding{}, errors.New("native runtime offline before creation")
				}
				return envelope.SessionBinding{Agent: agent, Host: nativeHost, Project: project, SessionID: "native-" + host}, nil
			}})
			body := fmt.Sprintf(`{"agent":"muse","host":%q,"project":"/new-project"}`, host)
			for i := 0; i < 3; i++ {
				r := httptest.NewRequest("POST", "/v1/project-session", strings.NewReader(body))
				r.RemoteAddr = grokAddr
				r.Header.Set(client.AgentHeader, "grokbot")
				r.Header.Set(client.SessionHeader, "same-origin-owner")
				w := httptest.NewRecorder()
				h.h.ServeHTTP(w, r)
				if (i == 0 && w.Code == 200) || (i > 0 && w.Code != 200) {
					t.Fatalf("attempt %d: status %d", i, w.Code)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("existing owner was not reused: %d provisioner calls", calls.Load())
			}
		})
	}
}
