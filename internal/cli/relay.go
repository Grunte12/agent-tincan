package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"tailscale.com/tsnet"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/gateway"
	"github.com/mvanhorn/agent-tincan/internal/identity"
	"github.com/mvanhorn/agent-tincan/internal/policy"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
	"github.com/mvanhorn/agent-tincan/internal/wake"
)

type relayFlags struct {
	urgentPerHour int
	listen        string
	hostname      string
	stateDir      string
	port          int
	admins        []string
	adminLogins   []string
	noRebind      bool
	replyGrace    time.Duration
	dist          string
	upgradeExit   bool
	releaseURL    string

	gateway         bool
	gatewayHostname string
	gatewayListen   string
	gatewayURL      string
}

func relayCmd() *cobra.Command {
	var f relayFlags
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Run the relay (on the always-on machine, e.g. the Grok Bot VM)",
		Long: `Run the relay. By default it joins the tailnet as its own node with tsnet
(set TS_AUTHKEY, or follow the login URL it prints). With --listen it binds
this host's tailnet IP and uses the host's tailscaled instead.

Admin commands (invite, remove) are accepted from the local admin socket in
the state dir and from machines named in --admin that carry no Tailscale tags
(and, with --admin-login, are owned by a listed login). Tag agent machines
(e.g. tag:agent) so they can never be admins.

Wake settings (webhook URLs, email addresses, keys) live in wake.json in the
state dir, chmod 600. They are never sent to agents. A webhook or email agent
is also woken when a reply to its own request is still unread after
--reply-grace.

A rebuilt machine (a new Tailscale node with the same machine name, or that
name plus a "-1" style suffix) is re-admitted as its old agent on its first
call when it is untagged, owned by the login recorded at join, and the old
node is offline or gone. Each one is audited as a "rebind" event. Turn this
off with --no-auto-rebind.

With --dist <dir>, the relay serves tincan release binaries from dir to joined
agents and admins, so tincan upgrade works on machines without GitHub access.
Put the raw binaries there as tincan_<os>_<arch> (linux or darwin, amd64 or
arm64), plus checksums.txt and a VERSION file naming the release. An admin can
then upgrade the relay itself with tincan relay-upgrade: it installs the dist
build over this binary (which the relay user must own) and re-executes, or
with --upgrade-exit exits with status 75 for its supervisor to restart it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.urgentPerHour < 1 {
				return fmt.Errorf("--urgent-per-hour must be at least 1, got %d", f.urgentPerHour)
			}
			if f.releaseURL != "" && !strings.HasPrefix(f.releaseURL, "https://") {
				return fmt.Errorf("--release-url must be an https URL, got %q", f.releaseURL)
			}
			err := runRelay(cmd.Context(), f)
			if rs, ok := errors.AsType[*errRelayRestart](err); ok {
				return rs.u.restart()
			}
			return err
		},
	}
	cmd.Flags().StringVar(&f.listen, "listen", "", "bind this host tailnet IP (100.x.y.z) instead of starting tsnet")
	cmd.Flags().StringVar(&f.hostname, "hostname", "tincan-relay", "tsnet node name")
	cmd.Flags().StringVar(&f.stateDir, "state-dir", defaultStateDir(), "relay state: database, tsnet state, admin socket")
	cmd.Flags().IntVar(&f.port, "port", 80, "port to serve the agent API on")
	cmd.Flags().StringSliceVar(&f.admins, "admin", nil, "machine names allowed to run admin commands (e.g. macbook-pro-44,iphone182)")
	cmd.Flags().StringSliceVar(&f.adminLogins, "admin-login", nil, "if set, admin machines must also be owned by one of these Tailscale logins")
	cmd.Flags().BoolVar(&f.noRebind, "no-auto-rebind", false, "do not re-admit rebuilt machines automatically; they need a new invite")
	cmd.Flags().IntVar(&f.urgentPerHour, "urgent-per-hour", 5, "maximum urgent requests per sender per hour")
	cmd.Flags().DurationVar(&f.replyGrace, "reply-grace", wake.DefaultReplyGrace, "how long a reply may go unread before a webhook or email agent is woken to read it")
	cmd.Flags().StringVar(&f.dist, "dist", "", "serve tincan release binaries (tincan_<os>_<arch>, checksums.txt, VERSION) from this directory for tincan upgrade")
	cmd.Flags().BoolVar(&f.upgradeExit, "upgrade-exit", false, "after tincan relay-upgrade, exit with status 75 for a supervisor to restart the relay instead of re-executing it")
	cmd.Flags().StringVar(&f.releaseURL, "release-url", "", "let tincan relay-upgrade --from-github download releases from <url>/<tag>/<file> (for this project: "+GitHubReleaseURL+"); off when empty")
	cmd.Flags().BoolVar(&f.gateway, "chatgpt-gateway", false, "serve the public ChatGPT MCP gateway through Tailscale Funnel (OAuth-protected)")
	cmd.Flags().StringVar(&f.gatewayHostname, "gateway-hostname", "tincan-gateway", "tsnet node name for the Funnel gateway")
	cmd.Flags().StringVar(&f.gatewayListen, "gateway-listen", "", "serve the gateway on this plain-HTTP address instead of Funnel (put your own TLS proxy in front)")
	cmd.Flags().StringVar(&f.gatewayURL, "gateway-url", "", "public https URL of the gateway when using --gateway-listen")
	return cmd
}

// directoryConfig maps the relay flags onto the identity directory.
func (f relayFlags) directoryConfig() identity.Config {
	return identity.Config{Admins: f.admins, AdminLogins: f.adminLogins, NoAutoRebind: f.noRebind}
}

func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "tincan-relay")
	}
	return ".tincan-relay"
}

// listenAddr parses --listen: a tailnet IPv4 address (100.x.y.z), with
// an optional :port that overrides port. Anything else is refused, so a
// typo fails before the relay creates its state.
func listenAddr(listen string, port int) (string, error) {
	bad := fmt.Errorf("--listen must be a tailnet 100.x address, got %q", listen)
	host := strings.TrimSpace(listen)
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", bad
		}
		host, port = h, n
	}
	ip := net.ParseIP(host).To4()
	if ip == nil || ip[0] != 100 || strings.Contains(host, ":") {
		return "", bad
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func runRelay(ctx context.Context, f relayFlags) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Parse --listen fully before creating anything in the state dir.
	var listenAt string
	if f.listen != "" {
		addr, err := listenAddr(f.listen, f.port)
		if err != nil {
			return err
		}
		listenAt = addr
	}
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(f.stateDir, "relay.db"))
	if err != nil {
		return err
	}
	closeStore := sync.OnceFunc(func() {
		if err := st.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	})
	defer closeStore()
	approval, err := policy.LoadApproval(filepath.Join(f.stateDir, "approval.json"))
	if err != nil {
		return err
	}

	ln, who, closeNetFn, err := openRelayNet(ctx, f, listenAt)
	if err != nil {
		return err
	}
	closeNet := sync.OnceFunc(closeNetFn)
	defer closeNet()
	if len(f.admins) == 0 {
		log.Printf("no --admin machines set: invites only work from the local admin socket")
	}

	dir := identity.NewDirectory(st, identity.WithVirtual(who), f.directoryConfig())
	srv := relay.New(dir, st, relay.Config{Version: Version})
	urls := who.SelfURLs(ctx, f.port)
	srv.SetURLs(urls)
	log.Printf("tincan relay advertises %s to its agents", strings.Join(urls, ", "))
	if f.listen != "" {
		log.Printf("warning: with --listen the relay's address is this host's tailnet address, which changes if the host re-joins Tailscale. "+
			"Agents with tailscale find it again by themselves; proxy-only agents may need tincan rejoin. "+
			"Without --listen the relay is its own tailnet node (%s) and keeps its name while --state-dir is kept.", f.hostname)
	}
	srv.SetPreparer(policy.New(st, policy.Config{Approval: approval, UrgentPerHour: f.urgentPerHour}))
	if f.dist != "" {
		if fi, err := os.Stat(f.dist); err != nil || !fi.IsDir() {
			return fmt.Errorf("--dist %s: not a directory", f.dist)
		}
		srv.SetDist(f.dist)
	}
	ctx, up := withRelayUpgrader(ctx, srv, f)
	wakeCfg, err := wake.LoadConfig(filepath.Join(f.stateDir, "wake.json"))
	if err != nil {
		return err
	}
	waker := wake.New(wakeCfg, st, wake.Options{Online: srv.Online, Queued: srv.QueuedCount, UnseenReplies: srv.UnseenReplies, ReplyGrace: f.replyGrace})
	srv.SetEvents(waker)
	srv.SetWakeNamer(waker)
	if err := resumeReplyWakes(ctx, st, waker); err != nil {
		log.Printf("reschedule reply wakes: %v", err) // replies stay unseen for the agent's next check
	}
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		srv.Run(runCtx)
	}()

	api := client.Configure(&http.Server{Handler: srv.Handler()}, client.RelayAPI)
	adminSock := filepath.Join(f.stateDir, "admin.sock")
	os.Remove(adminSock)
	aln, err := client.ListenUnix(adminSock)
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	if err := os.Chmod(adminSock, 0o600); err != nil {
		return err
	}
	admin := client.Configure(&http.Server{Handler: srv.AdminHandler()}, client.RelayAPI)

	errc := make(chan error, 3)
	servers := []*http.Server{api, admin}
	closeGW := func() {}
	go func() { errc <- api.Serve(ln) }()
	go func() { errc <- admin.Serve(aln) }()
	if f.gateway {
		gln, base, closeGWFn, err := gatewayListener(ctx, f)
		if err != nil {
			return fmt.Errorf("chatgpt gateway: %w", err)
		}
		closeGW = sync.OnceFunc(closeGWFn)
		defer closeGW()
		oauth, err := gateway.NewOAuth(st.DB(), nil)
		if err != nil {
			return err
		}
		srv.SetConnector(gateway.Connector{Dir: dir, OAuth: oauth, Base: base})
		gw := client.Configure(&http.Server{Handler: gateway.New(base, oauth, srv.Handler(), Version).Handler()}, client.RelayAPI)
		go func() { errc <- gw.Serve(gln) }()
		servers = append(servers, gw)
		log.Printf("chatgpt gateway serving at %s/mcp", base)
	}
	log.Printf("tincan relay serving on %s (admin socket %s)", ln.Addr(), adminSock)

	var served error
	select {
	case <-ctx.Done():
	case served = <-errc:
		if errors.Is(served, http.ErrServerClosed) {
			served = nil
		}
	}
	// From here a second SIGINT or SIGTERM gets the default handling and
	// ends the process at once.
	stop()
	log.Printf("tincan relay shutting down")
	var step atomic.Value
	watchdog := time.AfterFunc(relayExitTimeout, func() {
		log.Printf("tincan relay: shutdown stuck in %s after %s; exiting now", step.Load(), relayExitTimeout)
		os.Exit(1)
	})
	defer watchdog.Stop()
	for _, s := range []struct {
		name string
		do   func()
	}{
		// Held long polls and waits answer "nothing yet" first, so the
		// drain below only waits for calls that are really working.
		{"ending held polls", srv.Stop},
		{"draining connections", func() { shutdown(servers) }},
		{"stopping sweeps", func() { stopRun(); <-ran }},
		{"stopping wakes", waker.Stop},
		{"closing the gateway", closeGW},
		{"closing the tailnet listener", closeNet},
		{"closing the store", closeStore},
	} {
		step.Store(s.name)
		s.do()
	}
	log.Printf("tincan relay stopped")
	if served != nil {
		return served
	}
	// After tincan relay-upgrade: the store and listeners are closed, so
	// the caller can now re-exec the new build or exit for a supervisor.
	return up.restartErr()
}

// relayDrainTimeout bounds how long shutdown waits for calls in flight
// before closing their connections. Held long polls do not count: they
// answer as soon as shutdown starts.
var relayDrainTimeout = 10 * time.Second

// relayExitTimeout bounds the whole shutdown. A relay still stuck after it
// (a tailnet node that will not close, say) logs the step and exits anyway.
var relayExitTimeout = relayDrainTimeout + 10*time.Second

// shutdown drains servers, closing whatever is still open when
// relayDrainTimeout runs out.
func shutdown(servers []*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), relayDrainTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(ctx); err != nil {
				s.Close()
			}
		})
	}
	wg.Wait()
}

// relayResolver is what the relay needs from its tailnet: caller identity,
// node liveness for rebinds, and the URLs it advertises.
type relayResolver interface {
	identity.Resolver
	identity.NodeStatus
	SelfURLs(ctx context.Context, port int) []string
}

// openRelayNet opens the relay's agent API listener and identity resolver:
// the host tailnet address through tailscaled with --listen, or its own
// tsnet node. Tests replace it to serve on loopback with a fake tailnet.
var openRelayNet = func(ctx context.Context, f relayFlags, listenAt string) (net.Listener, relayResolver, func(), error) {
	if f.listen != "" {
		who := identity.NewLocalResolverAt("")
		if err := who.Probe(ctx); err != nil {
			return nil, nil, nil, fmt.Errorf("refusing to start without WhoIs: %w", err)
		}
		ln, err := net.Listen("tcp", listenAt)
		if err != nil {
			return nil, nil, nil, err
		}
		return ln, who, func() {}, nil
	}
	ts := &tsnet.Server{Hostname: f.hostname, Dir: filepath.Join(f.stateDir, "tsnet"), AuthKey: os.Getenv("TS_AUTHKEY")}
	fail := func(err error) (net.Listener, relayResolver, func(), error) {
		ts.Close()
		return nil, nil, nil, err
	}
	if _, err := ts.Up(ctx); err != nil {
		return fail(fmt.Errorf("tsnet up: %w", err))
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return fail(err)
	}
	ln, err := ts.Listen("tcp", fmt.Sprintf(":%d", f.port))
	if err != nil {
		return fail(err)
	}
	return ln, identity.NewLocalResolver(lc), func() { ts.Close() }, nil
}

// gatewayListener returns the public listener and base URL for the ChatGPT
// gateway: a Funnel listener on its own tsnet node by default.
func gatewayListener(ctx context.Context, f relayFlags) (net.Listener, string, func(), error) {
	if f.gatewayListen != "" {
		if f.gatewayURL == "" {
			return nil, "", nil, fmt.Errorf("--gateway-url is required with --gateway-listen")
		}
		ln, err := net.Listen("tcp", f.gatewayListen)
		return ln, f.gatewayURL, func() {}, err
	}
	ts := &tsnet.Server{Hostname: f.gatewayHostname, Dir: filepath.Join(f.stateDir, "tsnet-gateway"), AuthKey: os.Getenv("TS_AUTHKEY")}
	if _, err := ts.Up(ctx); err != nil {
		return nil, "", nil, err
	}
	ln, err := ts.ListenFunnel("tcp", ":443")
	if err != nil {
		ts.Close()
		return nil, "", nil, fmt.Errorf("%w (Funnel needs HTTPS certificates and the funnel node attribute in your tailnet policy)", err)
	}
	domains := ts.CertDomains()
	if len(domains) == 0 {
		ts.Close()
		return nil, "", nil, errors.New("no HTTPS domain for the gateway node; enable HTTPS certificates in the Tailscale admin console")
	}
	return ln, "https://" + domains[0], func() { ts.Close() }, nil
}

// resumeReplyWakes schedules a reply wake for every agent that still holds
// unseen replies. The waker keeps its grace-period timers in memory, so
// without this a relay restart inside the grace window would never wake the
// asker. Each wake still waits out the grace period and is dropped if the
// reply was read by then.
func resumeReplyWakes(ctx context.Context, st *store.Store, w *wake.Waker) error {
	agents, err := st.AgentsWithUnseenReplies(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		w.ReplyWaiting(a)
	}
	return nil
}
