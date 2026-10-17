package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func catalog(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionCatalogResolvesOnlyUnambiguousProject(t *testing.T) {
	s := catalog(t)
	ctx := t.Context()
	p := "/workspace/app"
	for _, id := range []string{"synthetic-a", "synthetic-b"} {
		got, changed, err := s.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "muse", Project: p, SessionID: id})
		if err != nil || !changed || got.SessionID != id {
			t.Fatalf("discover %q: %+v changed=%v err=%v", id, got, changed, err)
		}
	}
	if _, _, err := s.ResolveProjectSession(ctx, "grokbot", "muse", p); !errors.Is(err, ErrSessionOwnerAmbiguous) {
		t.Fatalf("ambiguous resolve = %v", err)
	}
	single := catalog(t)
	first, changed, err := single.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "muse", Project: p, SessionID: "synthetic-owner"})
	if err != nil || !changed {
		t.Fatalf("discover: %+v %v %v", first, changed, err)
	}
	// A later cwd change for the same native session must keep its first project.
	retained, changed, err := single.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "muse", Project: "/workspace/other", SessionID: "synthetic-owner"})
	if err != nil || changed || retained.Project != p {
		t.Fatalf("cwd changed original binding: %+v changed=%v err=%v", retained, changed, err)
	}
	resolved, changed, err := single.ResolveProjectSession(ctx, "grokbot", "muse", p)
	if err != nil || !changed || resolved.SessionID != "synthetic-owner" {
		t.Fatalf("resolve: %+v changed=%v err=%v", resolved, changed, err)
	}
	if again, changed, err := single.ResolveProjectSession(ctx, "grokbot", "muse", p); err != nil || changed || again != resolved {
		t.Fatalf("owner not stable: %+v changed=%v err=%v", again, changed, err)
	}
}

func TestSessionCatalogPreservedOwnerBindingAndConflicts(t *testing.T) {
	s := catalog(t)
	ctx := t.Context()
	unresolved := envelope.SessionBinding{Agent: "grokbot", Host: "codex", SessionID: "legacy-owner"}
	if changed, err := s.PreserveSessionOwner(ctx, unresolved); err != nil || !changed {
		t.Fatalf("preserve: changed=%v err=%v", changed, err)
	}
	if changed, err := s.PreserveSessionOwner(ctx, unresolved); err != nil || changed {
		t.Fatalf("idempotent preserve: changed=%v err=%v", changed, err)
	}
	if _, _, err := s.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "codex", Project: "/workspace/legacy", SessionID: "unrelated-chat"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveProjectSession(ctx, "grokbot", "codex", "/workspace/legacy"); !errors.Is(err, ErrSessionOwnerPreserved) {
		t.Fatalf("unresolved pin = %v", err)
	}
	got, changed, err := s.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "codex", Project: "/workspace/legacy", SessionID: "legacy-owner"})
	if err != nil || !changed || got.SessionID != unresolved.SessionID {
		t.Fatalf("native event failed to bind legacy owner: %+v changed=%v err=%v", got, changed, err)
	}
	resolved, changed, err := s.ResolveProjectSession(ctx, "grokbot", "codex", "/workspace/legacy")
	if err != nil || changed || resolved.SessionID != unresolved.SessionID {
		t.Fatalf("resolved preserved owner: %+v changed=%v err=%v", resolved, changed, err)
	}
	if changed, err := s.PreserveSessionOwner(ctx, unresolved); err != nil || changed {
		t.Fatalf("re-import unresolved pin after materialization: changed=%v err=%v", changed, err)
	}
	if _, _, err := s.DiscoverSession(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "codex", Project: "/workspace/legacy", SessionID: "intruder"}); err != nil {
		t.Fatalf("record extra discovery: %v", err)
	}
	if _, _, err := s.ResolveProjectSession(ctx, "grokbot", "codex", "/workspace/legacy"); err != nil {
		t.Fatalf("immutable owner lost to later discovery: %v", err)
	}
	if _, err := s.PreserveSessionOwner(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "codex", Project: "/workspace/legacy", SessionID: "different"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("changed pin = %v", err)
	}
	if _, err := s.PreserveSessionOwner(ctx, envelope.SessionBinding{Agent: "grokbot", Host: "codex", Project: "/workspace/elsewhere", SessionID: "legacy-owner"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("changed pinned project = %v", err)
	}
}

func TestSessionCatalogConcurrentDiscoveryAndDurableOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	binding := envelope.SessionBinding{Agent: "muse", Host: "cursor", Project: "/workspace/concurrent", SessionID: "stable-chat"}
	const workers = 12
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, changed, err := s.DiscoverSession(ctx, binding)
			results <- changed
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	changes := 0
	for changed := range results {
		if changed {
			changes++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent discovery: %v", err)
		}
	}
	if changes != 1 {
		t.Fatalf("changed discoveries=%d, want 1", changes)
	}
	if _, changed, err := s.ResolveProjectSession(ctx, binding.Agent, binding.Host, binding.Project); err != nil || !changed {
		t.Fatalf("resolve owner: changed=%v err=%v", changed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, changed, err := s.ResolveProjectSession(ctx, binding.Agent, binding.Host, binding.Project)
	if err != nil || changed || got != binding {
		t.Fatalf("reopened owner: %+v changed=%v err=%v", got, changed, err)
	}
}

func TestSessionCatalogPreservesMultipleProjectsAndUnknownPin(t *testing.T) {
	s := catalog(t)
	ctx := t.Context()
	knownA := envelope.SessionBinding{Agent: "muse", Host: "muse", Project: "/workspace/alpha", SessionID: "alpha-owner"}
	knownB := envelope.SessionBinding{Agent: "muse", Host: "muse", Project: "/workspace/beta", SessionID: "beta-owner"}
	if changed, err := s.PreserveSessionOwner(ctx, knownA); err != nil || !changed {
		t.Fatalf("preserve alpha: changed=%v err=%v", changed, err)
	}
	if changed, err := s.PreserveSessionOwner(ctx, knownB); err != nil || !changed {
		t.Fatalf("preserve beta on same host: changed=%v err=%v", changed, err)
	}
	for _, want := range []envelope.SessionBinding{knownA, knownB} {
		got, changed, err := s.ResolveProjectSession(ctx, want.Agent, want.Host, want.Project)
		if err != nil || changed || got != want {
			t.Errorf("resolve %s: got=%+v changed=%v err=%v", want.Project, got, changed, err)
		}
	}
	if _, err := s.PreserveSessionOwner(ctx, envelope.SessionBinding{Agent: "muse", Host: "muse", Project: knownA.Project, SessionID: "replacement"}); !errors.Is(err, ErrWrongState) {
		t.Fatalf("overwritten known owner: %v", err)
	}

	unknown := envelope.SessionBinding{Agent: "grokbot", Host: "cursor", SessionID: "preexisting-chat"}
	if changed, err := s.PreserveSessionOwner(ctx, unknown); err != nil || !changed {
		t.Fatalf("preserve unknown pin: changed=%v err=%v", changed, err)
	}
	known := envelope.SessionBinding{Agent: unknown.Agent, Host: unknown.Host, Project: "/workspace/known", SessionID: "known-chat"}
	if changed, err := s.PreserveSessionOwner(ctx, known); err != nil || !changed {
		t.Fatalf("known project blocked by unknown pin: changed=%v err=%v", changed, err)
	}
	if got, changed, err := s.ResolveProjectSession(ctx, known.Agent, known.Host, known.Project); err != nil || changed || got != known {
		t.Fatalf("known project resolve: %+v changed=%v err=%v", got, changed, err)
	}
	if _, _, err := s.ResolveProjectSession(ctx, unknown.Agent, unknown.Host, "/workspace/unknown"); !errors.Is(err, ErrSessionOwnerPreserved) {
		t.Fatalf("unknown pin was lost or guessed: %v", err)
	}
	if changed, err := s.PreserveSessionOwner(ctx, envelope.SessionBinding{Agent: unknown.Agent, Host: unknown.Host, SessionID: "different-chat"}); !errors.Is(err, ErrWrongState) || changed {
		t.Fatalf("unknown pin replaced: changed=%v err=%v", changed, err)
	}
	if _, _, err := s.DiscoverSession(ctx, envelope.SessionBinding{Agent: unknown.Agent, Host: unknown.Host, Project: "/workspace/unknown", SessionID: unknown.SessionID}); err != nil {
		t.Fatalf("discover pinned chat: %v", err)
	}
	if got, changed, err := s.ResolveProjectSession(ctx, unknown.Agent, unknown.Host, "/workspace/unknown"); err != nil || changed || got.SessionID != unknown.SessionID {
		t.Fatalf("resolve materialized unknown pin: %+v changed=%v err=%v", got, changed, err)
	}
}

func TestSessionCatalogRejectsInvalidBindings(t *testing.T) {
	s := catalog(t)
	base := envelope.SessionBinding{Agent: "grokbot", Host: "muse", Project: "/workspace/app", SessionID: "synthetic"}
	invalid := []envelope.SessionBinding{
		{Agent: "../bad", Host: base.Host, Project: base.Project, SessionID: base.SessionID},
		{Agent: base.Agent, Host: "legacy", Project: base.Project, SessionID: base.SessionID},
		{Agent: base.Agent, Host: base.Host, Project: "/workspace/../app", SessionID: base.SessionID},
		{Agent: base.Agent, Host: base.Host, Project: "/workspace/with\x00nul", SessionID: base.SessionID},
		{Agent: base.Agent, Host: base.Host, Project: "/" + strings.Repeat("x", 4096), SessionID: base.SessionID},
		{Agent: base.Agent, Host: base.Host, Project: base.Project, SessionID: "bad/id"},
	}
	for i, binding := range invalid {
		if _, _, err := s.DiscoverSession(context.Background(), binding); err == nil {
			t.Errorf("invalid binding %d accepted", i)
		}
	}
	if _, _, err := s.ResolveProjectSession(context.Background(), base.Agent, base.Host, "relative"); err == nil {
		t.Fatal("relative project accepted")
	}
}

func TestRegisteredSessionAndRetirementPreventStaleRouting(t *testing.T) {
	s := catalog(t)
	ctx := context.Background()
	binding := envelope.SessionBinding{Agent: "claude-code", Host: "claude-code", Project: "/workspace/video", SessionID: "00000000-0000-4000-8000-000000000123"}
	if _, _, err := s.DiscoverSession(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RegisteredSession(ctx, binding.Agent, binding.SessionID); err != nil || got != binding {
		t.Fatalf("registered session = %+v, %v", got, err)
	}
	if _, err := s.RegisteredSession(ctx, "grokbot", binding.SessionID); !errors.Is(err, ErrSessionOwnerMissing) {
		t.Fatalf("cross-agent exact route: %v", err)
	}
	changed, err := s.RetireSession(ctx, binding)
	if err != nil || !changed {
		t.Fatalf("retire changed=%v err=%v", changed, err)
	}
	if _, err := s.RegisteredSession(ctx, binding.Agent, binding.SessionID); !errors.Is(err, ErrSessionOwnerMissing) {
		t.Fatalf("retired session still resolves: %v", err)
	}
}

func TestRetirementRefusesToOrphanActiveWork(t *testing.T) {
	s := catalog(t)
	ctx := context.Background()
	binding := envelope.SessionBinding{Agent: "claude-code", Host: "claude-code", Project: "/workspace/video", SessionID: "00000000-0000-4000-8000-000000000124"}
	if _, _, err := s.DiscoverSession(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, envelope.Request{From: "grokbot", To: binding.Agent, ToSession: binding.SessionID, Kind: envelope.KindAsk, Body: "work", Chain: []string{"grokbot"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RetireSession(ctx, binding); !errors.Is(err, ErrSessionHasActiveWork) || changed {
		t.Fatalf("active retirement changed=%v err=%v", changed, err)
	}
	if _, err := s.RegisteredSession(ctx, binding.Agent, binding.SessionID); err != nil {
		t.Fatalf("active route was removed: %v", err)
	}
}
