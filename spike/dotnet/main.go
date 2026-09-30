// Command spike-dotnet is the throwaway U1 check for the dot's sandbox. The
// dot's cloud computer blocks NETLINK_ROUTE sockets, so stock tsnet dies in
// netmon.New while listing interfaces. This program registers a static
// interface getter (one up interface with a private address and non-nil
// AltAddrs, so the netlink-backed Addrs is never called), disables netns,
// joins the tailnet with a tagged single-use auth key from TS_AUTHKEY, and
// does GET <relay>/v1/hello over the node. It prints a stage marker at each
// step so the owner can record exactly where it failed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	// tsnet already links this; imported explicitly so HTTPS_PROXY support
	// (feature.HookProxyFromEnvironment) cannot silently drop out.
	_ "tailscale.com/feature/useproxy"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
)

func main() {
	hostname := flag.String("hostname", "dot-spike", "tsnet node name")
	dir := flag.String("dir", "", "tsnet state dir (default: a new temp dir)")
	relay := flag.String("relay", "http://tincan-relay", "relay base URL on the tailnet")
	ifaceName := flag.String("iface-name", "eth0", "name of the static interface reported to netmon")
	ifaceIP := flag.String("iface-ip", "10.0.0.2", "private IPv4 reported on the static interface")
	timeout := flag.Duration("timeout", 2*time.Minute, "how long to wait for the node to come up")
	verbose := flag.Bool("v", false, "print verbose tailscale backend logs (auth key redacted)")
	flag.Parse()
	log.SetFlags(log.Ltime)

	authKey := os.Getenv("TS_AUTHKEY")
	if authKey == "" {
		fail("config", errors.New("TS_AUTHKEY is not set; this spike only joins with a tagged single-use auth key and never uses an interactive login"))
	}
	logf := redactingLogf(authKey)

	if err := registerStaticInterface(*ifaceName, *ifaceIP); err != nil {
		fail("netmon", err)
	}
	netns.SetEnabled(false)
	var seen []string
	if err := netmon.ForeachInterface(func(ni netmon.Interface, pfxs []netip.Prefix) {
		seen = append(seen, fmt.Sprintf("%s%v", ni.Name, pfxs))
	}); err != nil {
		fail("netmon", err)
	}
	dr, drErr := netmon.DefaultRouteInterface() // informational; tsnet ignores its error
	fmt.Printf("stage: netmon ok (interfaces %s; default route %q, err=%v)\n", strings.Join(seen, " "), dr, drErr)
	printProxyEnv()

	stateDir := *dir
	if stateDir == "" {
		d, err := os.MkdirTemp("", "dot-spike-")
		if err != nil {
			fail("config", err)
		}
		stateDir = d
	}
	fmt.Printf("stage: state dir %s\n", stateDir)

	srv := &tsnet.Server{
		Hostname: *hostname,
		Dir:      stateDir,
		AuthKey:  authKey,
		UserLogf: func(format string, args ...any) {
			// Never follow or print an interactive login URL: it would make an
			// untagged node owned by the owner that can reach the whole tailnet.
			if strings.Contains(fmt.Sprintf(format, args...), "go to: http") {
				fail("login", errors.New("control asked for an interactive login; refusing (the auth key was rejected, used, or expired)"))
			}
			logf(format, args...)
		},
	}
	if *verbose {
		srv.Logf = logf
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		fail("start", err)
	}
	fmt.Println("stage: start ok")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	st, err := srv.Up(ctx)
	if err != nil {
		printBackendState(srv)
		fail("up", err)
	}
	var ips []string
	if st.Self != nil {
		for _, ip := range st.Self.TailscaleIPs {
			ips = append(ips, ip.String())
		}
	}
	fmt.Printf("stage: up (tailnet IPs %s)\n", strings.Join(ips, ", "))

	url := strings.TrimRight(*relay, "/") + "/v1/hello"
	hctx, hcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer hcancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, url, nil)
	if err != nil {
		fail("hello", err)
	}
	resp, err := srv.HTTPClient().Do(req)
	if err != nil {
		fail("hello", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	fmt.Printf("stage: hello %d\n%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode != 200 {
		os.Exit(1)
	}
	fmt.Println("stage: done")
}

// registerStaticInterface makes netmon see one up interface with a private
// IPv4 instead of asking the kernel over netlink.
func registerStaticInterface(name, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() || !addr.IsPrivate() {
		return fmt.Errorf("-iface-ip %q must be a private IPv4 address", ip)
	}
	ifc := &net.Interface{Index: 1, MTU: 1500, Name: name, Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning}
	ipnet := &net.IPNet{IP: net.IP(addr.AsSlice()), Mask: net.CIDRMask(24, 32)}
	netmon.RegisterInterfaceGetter(func() ([]netmon.Interface, error) {
		return []netmon.Interface{{Interface: ifc, AltAddrs: []net.Addr{ipnet}}}, nil
	})
	return nil
}

// printBackendState shows how far the node got (NeedsLogin, Starting, ...)
// and its health warnings, which name the control, DERP, or UDP problem.
func printBackendState(srv *tsnet.Server) {
	lc, err := srv.LocalClient()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return
	}
	key := os.Getenv("TS_AUTHKEY")
	fmt.Printf("backend state: %s\n", st.BackendState)
	for _, h := range st.Health {
		fmt.Printf("health: %s\n", redact(h, key))
	}
}

func printProxyEnv() {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "TS_FORCE_NOISE_443"} {
		if v := os.Getenv(k); v != "" {
			fmt.Printf("env: %s set (%s)\n", k, redactURL(v))
		}
	}
}

// redactURL drops any userinfo from a proxy URL before it is printed.
func redactURL(v string) string {
	if i := strings.Index(v, "@"); i >= 0 {
		if j := strings.Index(v, "://"); j >= 0 && j < i {
			return v[:j+3] + "REDACTED@" + v[i+1:]
		}
		return "REDACTED@" + v[i+1:]
	}
	return v
}

// redactingLogf never lets the auth key, or anything shaped like a
// tailscale key, reach the output.
func redactingLogf(authKey string) func(string, ...any) {
	return func(format string, args ...any) {
		log.Print(redact(fmt.Sprintf(format, args...), authKey))
	}
}

func redact(s, authKey string) string {
	if authKey != "" {
		s = strings.ReplaceAll(s, authKey, "[TS_AUTHKEY]")
	}
	for _, p := range []string{"tskey-", "nodekey:", "privkey:", "discokey:", "mkey:"} {
		for i := strings.Index(s, p); i >= 0; i = strings.Index(s, p) {
			end := i + len(p)
			for end < len(s) && !strings.ContainsRune(" \t\n,\"'}]", rune(s[end])) {
				end++
			}
			s = s[:i] + "[" + strings.TrimRight(p, ":-") + " redacted]" + s[end:]
		}
	}
	return s
}

func fail(stage string, err error) {
	fmt.Printf("stage: FAILED at %s: %s\n", stage, redact(err.Error(), os.Getenv("TS_AUTHKEY")))
	os.Exit(1)
}
