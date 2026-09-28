package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

// Every relay call names the build this client runs, once main has set
// client.Version; a program that never sets it sends no header. The roster
// decodes the relay's own build alongside the agents.
func TestClientSendsItsVersion(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agents": []client.AgentInfo{{Name: "muse", Version: "0.5.1"}}, "relay_version": "0.5.2",
		})
	}))
	defer srv.Close()

	t.Cleanup(func() { client.Version = "" })
	client.Version = "0.5.2"
	r, err := client.NewRelayFor(client.Config{Relay: srv.URL, Agent: "grokbot"})
	if err != nil {
		t.Fatal(err)
	}
	ro, err := r.Roster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Get(client.VersionHeader) != "0.5.2" || got.Get(client.AgentHeader) != "grokbot" {
		t.Fatalf("headers = %v", got)
	}
	if ro.RelayVersion != "0.5.2" || len(ro.Agents) != 1 || ro.Agents[0].Version != "0.5.1" {
		t.Fatalf("roster = %+v", ro)
	}

	client.Version = ""
	r, _ = client.NewRelayFor(client.Config{Relay: srv.URL, Agent: "grokbot"})
	if _, err := r.Agents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[client.VersionHeader]; ok {
		t.Fatalf("a client without a version sent %q", got.Get(client.VersionHeader))
	}
}

func TestNewer(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"0.5.5", "0.5.4", true}, {"v1.0.0", "0.99.99", true},
		{"0.10.0", "0.9.9", true}, {"0.5.5", "0.5.5", false},
		{"0.5.4", "0.5.5", false}, {"0.5.5+build", "0.5.5", false},
		{"0.5.5", "", false}, {"0.5.5", "dev", false},
		{"0.5.5", "0.5.4-3-gabcdef", false}, {"0.5.5", "0.5.4-dirty", false},
		{"0.5.5", "0.5.4-rc.1", false}, {"0.5.5-rc.2", "0.5.5-rc.1", true},
		{"0.5.5-rc.10", "0.5.5-rc.2", true}, {"0.5.5-rc.1", "0.5.4", true},
		{"0.5.5-rc.1", "0.5.5", false}, {"0.5.5-rc.01", "0.5.4", false},
		{"0.5.5-rc.1", "0.5.4-dev", false}, {"0.5.5-dev", "0.5.4", false},
		{"0.5", "0.4.0", false}, {"01.0.0", "0.4.0", false},
		{"0.5.5\nmalicious", "0.5.4", false},
		{"0.5.5-alpha.beta", "0.5.5-alpha.1", true},
		{"0.5.5-alpha.1", "0.5.5-alpha", true},
	} {
		if got := client.Newer(tt.a, tt.b); got != tt.want {
			t.Errorf("Newer(%q, %q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestConcurrentUpgradeNotice(t *testing.T) {
	old := client.Version
	client.Version = "0.5.4"
	t.Cleanup(func() { client.Version = old })
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := client.ReportUpgrade("95.0.0", func(string) error { calls.Add(1); return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("emitted %d times", calls.Load())
	}
}

func TestClientSendsItsPlatform(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(client.PlatformHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r, _ := client.NewRelay(srv.URL, "")
	_, _ = r.Peek(t.Context(), 0)
	if want := runtime.GOOS + "_" + runtime.GOARCH; <-got != want {
		t.Fatalf("platform header != %s", want)
	}
}
