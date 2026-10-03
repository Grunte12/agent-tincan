package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/paths"
	"tailscale.com/types/key"
)

// fakeRelay answers hello with a proof under key, and agents.
func fakeRelay(t *testing.T, key string) string {
	t.Helper()
	ts := httptest.NewServer(helloHandler(key))
	t.Cleanup(ts.Close)
	return ts.URL
}

// deadURL is an address where nothing listens any more.
func deadURL(t *testing.T) string {
	ts := httptest.NewServer(http.NotFoundHandler())
	u := ts.URL
	ts.Close()
	return u
}

func savedConfig(t *testing.T, c Config) {
	t.Helper()
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "client.json"))
	t.Setenv("TINCAN_RELAY", "")
	if err := SaveConfig(c); err != nil {
		t.Fatal(err)
	}
}

func TestRelayMovedIsFoundAndSaved(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	impostor := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key})
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key})
	if err != nil {
		t.Fatal(err)
	}
	r.findRelays = func(context.Context, string) []string { return []string{impostor, moved} }

	agents, err := r.Agents(t.Context())
	if err != nil || len(agents) != 1 {
		t.Fatalf("call after the move: %v %v", agents, err)
	}
	if r.Base() != moved {
		t.Fatalf("base %s, want %s (the peer that proved the key, not the impostor)", r.Base(), moved)
	}
	c, err := LoadConfig()
	if err != nil || c.Relay != moved || c.RelayKey != key {
		t.Fatalf("saved config %+v %v", c, err)
	}
	if len(c.RelayURLs) != 1 || c.RelayURLs[0] != "http://tincan-relay.example.ts.net" || c.RelayInfoAt.IsZero() {
		t.Fatalf("after the move, advertised URLs should be the live whoami list, got %+v", c)
	}
}

func TestRelayNotFollowedWithoutProof(t *testing.T) {
	old := deadURL(t)
	impostor := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, RelayKey: "k-real"})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: "k-real"})
	r.findRelays = func(context.Context, string) []string { return []string{impostor} }
	if _, err := r.Agents(t.Context()); err == nil {
		t.Fatal("followed a peer that cannot prove the relay key")
	}
	if r.Base() != old {
		t.Fatalf("base changed to %s", r.Base())
	}
}

func TestRelayNotSearchedWithoutKey(t *testing.T) {
	old := deadURL(t)
	savedConfig(t, Config{Relay: old})
	r, _ := NewRelayFor(Config{Relay: old})
	searched := false
	r.findRelays = func(context.Context, string) []string { searched = true; return nil }
	_, _ = r.Agents(t.Context())
	if searched {
		t.Fatal("searched the tailnet without a relay key")
	}
}

func TestRelayErrorIsNotAMove(t *testing.T) {
	if unreachable(&APIError{Code: 403, Message: "not a joined agent"}) {
		t.Fatal("an answer from the relay is not a move")
	}
}

func TestLearnRelayKeySavesIt(t *testing.T) {
	url := fakeRelay(t, "k-learned")
	savedConfig(t, Config{Relay: url, Agent: "muse"})
	r, _ := NewRelayFor(Config{Relay: url, Agent: "muse"})
	LearnRelayKey(t.Context(), r)
	raw, _ := os.ReadFile(ConfigPath())
	var c Config
	_ = json.Unmarshal(raw, &c)
	if c.RelayKey != "k-learned" || c.Relay != url {
		t.Fatalf("config %+v", c)
	}
}

func TestRelayFoundAtItsAdvertisedNameWithoutTailscale(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	named := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key, RelayURLs: []string{named}})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key, RelayURLs: []string{named}})
	r.findRelays = func(context.Context, string) []string { return nil } // a proxy-only sandbox
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != named {
		t.Fatalf("base %s, want the advertised %s", r.Base(), named)
	}
}

func TestProxyGatewayErrorsMeanUnreachable(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		if !unreachable(&APIError{Code: code}) {
			t.Errorf("%d through a proxy should count as the relay not answering", code)
		}
	}
}

func TestLearnRelayInfoSavesURLsAndRefreshes(t *testing.T) {
	url := fakeRelay(t, "k")
	savedConfig(t, Config{Relay: url})
	r, _ := NewRelayFor(Config{Relay: url})
	LearnRelayKey(t.Context(), r)
	c, _ := LoadConfig()
	if c.RelayKey != "k" || c.RelayInfoAt.IsZero() || NeedsRelayInfo(c) {
		t.Fatalf("config %+v", c)
	}
	c.RelayInfoAt = c.RelayInfoAt.Add(-2 * relayInfoEvery)
	if !NeedsRelayInfo(c) {
		t.Fatal("day-old relay info should be refreshed")
	}
}

// readConfig reads a config file as saved, with no environment overrides.
func readConfig(t *testing.T, path string) Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// A service loads its config from --config, not ConfigPath(). The key it
// learns and the address of a moved relay go to that file, and the default
// config (another agent's) is never touched.
func TestNewRelayForFileWritesToItsOwnFile(t *testing.T) {
	const key = "k-svc"
	url := fakeRelay(t, key)
	savedConfig(t, Config{Relay: url, Agent: "codex"}) // ConfigPath(): another agent
	own := filepath.Join(t.TempDir(), "history.json")
	if err := SaveConfigTo(own, Config{Relay: url, Agent: "history"}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRelayForFile(Config{Relay: url, Agent: "history"}, own)
	if err != nil {
		t.Fatal(err)
	}
	LearnRelayKey(t.Context(), r)
	if r.key != key {
		t.Fatalf("key learned %q", r.key)
	}
	if c := readConfig(t, own); c.RelayKey != key || c.Agent != "history" || len(c.RelayURLs) == 0 || c.RelayInfoAt.IsZero() {
		t.Fatalf("own config %+v", c)
	}
	if c := readConfig(t, ConfigPath()); c.RelayKey != "" || c.Agent != "codex" {
		t.Fatalf("ConfigPath() was written: %+v", c)
	}

	// The relay moves. Both files name the old address, so the old code
	// (which always wrote ConfigPath()) would have rewritten the wrong one.
	old := deadURL(t)
	if err := SaveConfig(Config{Relay: old, Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigTo(own, Config{Relay: old, Agent: "history", RelayKey: key}); err != nil {
		t.Fatal(err)
	}
	r, _ = NewRelayForFile(Config{Relay: old, Agent: "history", RelayKey: key}, own)
	r.findRelays = func(context.Context, string) []string { return []string{url} }
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := readConfig(t, own); c.Relay != url || c.RelayKey != key {
		t.Fatalf("own config after the move %+v", c)
	}
	if got := readConfig(t, own); len(got.RelayURLs) != 1 || got.RelayURLs[0] != "http://tincan-relay.example.ts.net" {
		t.Fatalf("own advertised URLs after the move %+v", got)
	}
	if c := readConfig(t, ConfigPath()); c.Relay != old {
		t.Fatalf("ConfigPath() followed the move for another agent: %+v", c)
	}
}

// TINCAN_RELAY overrides the saved relay for one process only: nothing is
// written to any file, whichever constructor built the client.
func TestEnvRelayOverrideNeverWrites(t *testing.T) {
	const key = "k-env"
	url := fakeRelay(t, key)
	savedConfig(t, Config{Relay: url, Agent: "muse"})
	own := filepath.Join(t.TempDir(), "history.json")
	if err := SaveConfigTo(own, Config{Relay: url, Agent: "history"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_RELAY", url)
	for name, build := range map[string]func() (*Relay, error){
		"NewRelayFor":     func() (*Relay, error) { return NewRelayFor(Config{Relay: url, Agent: "muse"}) },
		"NewRelayForFile": func() (*Relay, error) { return NewRelayForFile(Config{Relay: url, Agent: "history"}, own) },
	} {
		r, err := build()
		if err != nil {
			t.Fatal(err)
		}
		LearnRelayKey(t.Context(), r)
		if r.key != key {
			t.Fatalf("%s: the key is still handed out under TINCAN_RELAY, got %q", name, r.key)
		}
		if c := readConfig(t, ConfigPath()); c.RelayKey != "" {
			t.Fatalf("%s: ConfigPath() written under TINCAN_RELAY: %+v", name, c)
		}
		if c := readConfig(t, own); c.RelayKey != "" {
			t.Fatalf("%s: --config file written under TINCAN_RELAY: %+v", name, c)
		}
	}

	// A move is followed in memory but not written either.
	old := deadURL(t)
	t.Setenv("TINCAN_RELAY", old)
	if err := SaveConfigTo(own, Config{Relay: old, Agent: "history", RelayKey: key}); err != nil {
		t.Fatal(err)
	}
	r, _ := NewRelayForFile(Config{Relay: old, Agent: "history", RelayKey: key}, own)
	r.findRelays = func(context.Context, string) []string { return []string{url} }
	if _, err := r.Agents(t.Context()); err != nil || r.Base() != url {
		t.Fatalf("move: %v, base %s", err, r.Base())
	}
	if c := readConfig(t, own); c.Relay != old {
		t.Fatalf("--config file rewritten under TINCAN_RELAY: %+v", c)
	}
}

func TestIPv4sFromStatusJSONIncludesOfflineAndSkipsNames(t *testing.T) {
	raw := []byte(`{
		"Self": {"TailscaleIPs": ["100.98.147.65", "fd7a:115c:a1e0::1"], "HostName": "self-host", "Online": true},
		"Peer": {
			"n1": {"TailscaleIPs": ["100.96.137.127", "fd7a:115c:a1e0::2"], "HostName": "old-host", "Online": false},
			"n2": {"TailscaleIPs": ["100.67.11.14"], "HostName": "live-host", "Online": false},
			"n3": {"TailscaleIPs": ["100.1.2.3"], "HostName": "later-name", "Online": true},
			"n4": {"TailscaleIPs": ["fd7a:115c:a1e0::9"], "HostName": "v6-only", "Online": true}
		}
	}`)
	got, err := ipv4sFromStatusJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100.98.147.65", "100.96.137.127", "100.67.11.14", "100.1.2.3"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want every IPv4 including offline, not host names or IPv6", got)
	}
}

var _ = time.Second

func TestPeerURLsUseBasePortAndSkipCurrentHost(t *testing.T) {
	got := peerURLs("http://100.96.137.127:8787", []string{"100.96.137.127", "100.67.11.14", "100.1.2.3", "100.67.11.14"})
	want := []string{"http://100.67.11.14:8787", "http://100.1.2.3:8787"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTailnetCandidatesPrefersLocalAPIWhenCLIMissing(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return []string{"100.67.11.14"}, nil },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	got := tailnetCandidates(t.Context(), "http://100.96.137.127:8787")
	if !slices.Equal(got, []string{"http://100.67.11.14:8787"}) {
		t.Fatalf("got %v, want the LocalAPI address on the saved port", got)
	}
	ips, src := netmapIPv4s(t.Context())
	if src != "localapi" || !slices.Equal(ips, []string{"100.67.11.14"}) {
		t.Fatalf("netmap %v via %s", ips, src)
	}
}

func TestTailnetCandidatesFallsBackToCLI(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return nil, errors.New("no localapi") },
		func(context.Context) ([]string, error) { return []string{"100.67.11.14", "100.1.2.3"}, nil },
	))
	got := tailnetCandidates(t.Context(), "http://100.96.137.127:8787")
	want := []string{"http://100.67.11.14:8787", "http://100.1.2.3:8787"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	_, src := netmapIPv4s(t.Context())
	if src != "cli" {
		t.Fatalf("source %q, want cli", src)
	}
}

func TestTailnetCandidatesEmptyWithoutNetmap(t *testing.T) {
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) { return nil, errors.New("no localapi") },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	if got := tailnetCandidates(t.Context(), "http://100.96.137.127:8787"); got != nil {
		t.Fatalf("got %v, want none", got)
	}
	ips, src := netmapIPv4s(t.Context())
	if len(ips) != 0 || src != "" {
		t.Fatalf("netmap %v via %q", ips, src)
	}
}

func TestRelayMovedFromStaleIPFollowsProvingPeer(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	staleName := deadURL(t)
	moved := fakeRelay(t, key)
	other := fakeRelay(t, "k-other")
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key, RelayURLs: []string{staleName}})
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key, RelayURLs: []string{staleName}})
	if err != nil {
		t.Fatal(err)
	}
	r.findRelays = func(context.Context, string) []string { return []string{other, moved} }
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != moved {
		t.Fatalf("base %s, want the proving peer", r.Base())
	}
	c, _ := LoadConfig()
	if c.Relay != moved || slices.Contains(c.RelayURLs, staleName) {
		t.Fatalf("saved %+v still names the dead advertised URL", c)
	}
}

func TestRelocateRunsAfterCallerDeadline(t *testing.T) {
	const key = "k-real"
	old := deadURL(t)
	moved := fakeRelay(t, key)
	savedConfig(t, Config{Relay: old, RelayKey: key})
	r, _ := NewRelayFor(Config{Relay: old, RelayKey: key})
	r.findRelays = func(context.Context, string) []string { return []string{moved} }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !r.relocate(ctx, &net.OpError{Op: "dial", Err: errors.New("connection refused")}) {
		t.Fatal("a cancelled caller should not skip the search")
	}
	if r.Base() != moved {
		t.Fatalf("base %s", r.Base())
	}
}

func TestIPv4sFromLocalStatusIncludesOffline(t *testing.T) {
	st := &ipnstate.Status{
		Self: &ipnstate.PeerStatus{
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.98.147.65"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
			Online:       true,
		},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.96.137.127")}, Online: false, HostName: "old-host"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.67.11.14")}, Online: false, HostName: "live-host"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.1.2.3")}, Online: true, HostName: "later-name"},
			key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::9")}, Online: true},
		},
	}
	got := ipv4sFromLocalStatus(st)
	want := []string{"100.98.147.65", "100.96.137.127", "100.67.11.14", "100.1.2.3"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want every IPv4 including offline, not host names or IPv6", got)
	}
}

// swapLocalStatus makes localAPINetmap's LocalAPI call report whether it
// ran instead of dialing tailscaled.
func swapLocalStatus(t *testing.T, platform string) *bool {
	t.Helper()
	asked := new(bool)
	oldGOOS, oldStatus := goos, localStatus
	goos = platform
	localStatus = func(context.Context, *local.Client) (*ipnstate.Status, error) {
		*asked = true
		return &ipnstate.Status{}, nil
	}
	t.Cleanup(func() { goos, localStatus = oldGOOS, oldStatus })
	return asked
}

// TS_SOCKET naming a missing file means no tailscaled there: fail at once
// so a proxy 502 retry is not stalled on LocalAPI's dial timeout.
func TestLocalAPINetmapFailsFastWithMissingTSSocket(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		asked := swapLocalStatus(t, platform)
		t.Setenv("TS_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
		if _, err := localAPINetmap(t.Context()); err == nil || *asked {
			t.Fatalf("%s: err %v, asked LocalAPI %v; want an immediate miss", platform, err, *asked)
		}
	}
}

// On Linux the default socket is the only way to tailscaled, so a missing
// one fails at once too.
func TestLocalAPINetmapFailsFastWithoutLinuxSocket(t *testing.T) {
	if _, err := os.Stat(paths.DefaultTailscaledSocket()); err == nil {
		t.Skip("this machine has a tailscaled socket")
	}
	asked := swapLocalStatus(t, "linux")
	t.Setenv("TS_SOCKET", "")
	if _, err := localAPINetmap(t.Context()); err == nil || *asked {
		t.Fatalf("err %v, asked LocalAPI %v; want an immediate miss", err, *asked)
	}
}

// The macOS Tailscale app has no socket file; LocalAPI is a localhost TCP
// port with a token, which the local client finds by itself. A launchd
// service must still ask it rather than stop at the missing socket.
func TestLocalAPINetmapOnDarwinDoesNotNeedSocketFile(t *testing.T) {
	asked := swapLocalStatus(t, "darwin")
	t.Setenv("TS_SOCKET", "")
	if _, err := localAPINetmap(t.Context()); err != nil || !*asked {
		t.Fatalf("err %v, asked LocalAPI %v; want the darwin default to reach LocalAPI", err, *asked)
	}
}

// Production FindRelay path: LocalAPI IPs on the saved URL's port, no
// findRelays hook, follow only the peer that proves the key.
func TestRelayMovedFollowsLocalAPIAddresses(t *testing.T) {
	const key = "k-real"
	liveLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(liveLn.Addr().(*net.TCPAddr).Port)
	impLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		liveLn.Close()
		t.Skip("cannot bind 127.0.0.2")
	}
	serveHello(t, liveLn, key)
	serveHello(t, impLn, "k-other")
	old := "http://" + net.JoinHostPort("127.0.0.3", port)
	want := "http://" + net.JoinHostPort("127.0.0.1", port)
	savedConfig(t, Config{Relay: old, Agent: "muse", RelayKey: key})
	t.Cleanup(SwapNetmapLookups(
		func(context.Context) ([]string, error) {
			return []string{"127.0.0.3", "127.0.0.2", "127.0.0.1"}, nil
		},
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	r, err := NewRelayFor(Config{Relay: old, Agent: "muse", RelayKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agents(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.Base() != want {
		t.Fatalf("base %s, want the LocalAPI peer that proved the key", r.Base())
	}
	listed, src := r.LastFind()
	if src != "localapi" || listed != 2 {
		t.Fatalf("LastFind listed=%d source=%s, want 2 via localapi (current host skipped)", listed, src)
	}
}

func serveHello(t *testing.T, ln net.Listener, key string) {
	t.Helper()
	srv := &http.Server{Handler: helloHandler(key)}
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()
}

func helloHandler(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/hello":
			_ = json.NewEncoder(w).Encode(map[string]string{"service": HelloService, "proof": HelloProof(key, r.URL.Query().Get("nonce"))})
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "muse", "relay_key": key, "relay_urls": []string{"http://tincan-relay.example.ts.net"}})
		case "/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []AgentInfo{{Name: "muse"}}})
		default:
			http.NotFound(w, r)
		}
	})
}
