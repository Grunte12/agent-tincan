package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/council"
	"github.com/mvanhorn/agent-tincan/internal/history"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
)

// councilCmd is the Council service's command group: serve, install, and
// doctor run the service; the owner's commands sit beside them.
func councilCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "council",
		Short: "Run Council, which puts one question to every model on the team and returns a verdict",
		Long: "Council is a teammate that runs on the owner's machine: any teammate asks it a question, every\n" +
			"model on the team answers, the members rank each other's answers blind, and a chairman writes the\n" +
			"verdict. Set it up once with tincan council install, then check it with tincan council doctor.\n" +
			"See docs/adapters/council.md.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(councilServeCmd(), councilInstallCmd(), councilDoctorCmd())
	return cmd
}

// councilDir resolves the --dir flag: the service's own folder, where
// council.json and, by default, its database and reports live.
func councilDir(dir string) string {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = council.DefaultDir(runtime.GOOS, home)
	}
	return expandHome(dir)
}

const councilDirFlagHelp = "Council's own folder, holding council.json, its database, and reports (default: ~/Library/Application Support/tincan-council)"

func councilServeCmd() *cobra.Command {
	var configPath, dir string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run Council: claim convening requests over the relay and run councils one at a time",
		Long: "Long-polls the relay as the council agent. Each convening request is claimed at once and queued;\n" +
			"councils run one at a time, with a progress note at each stage. Council settings are read from\n" +
			"council.json in --dir. It refuses to run unless the relay stores kind council for this agent, since\n" +
			"only then does the relay hold agents' councils for the owner's approval.\n" +
			"Normally started by the service definition tincan council install writes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = serviceConfigPath(council.DefaultAgentName)
			}
			configPath = expandHome(configPath)
			cfg, err := client.LoadConfigFrom(configPath)
			if err != nil {
				return fmt.Errorf("council config %s: %w", configPath, err)
			}
			if cfg.Relay == "" {
				return fmt.Errorf("no relay configured in %s: run TINCAN_CONFIG=%s tincan join <code> --relay http://tincan-relay", configPath, configPath)
			}
			if cfg.Agent != "" && cfg.Agent != council.DefaultAgentName {
				return wrongServeAgent("council serve", council.DefaultAgentName, configPath+" is joined as", cfg.Agent)
			}
			dir = councilDir(dir)
			settings, err := council.LoadConfig(council.ConfigPathIn(dir))
			if err != nil {
				return err
			}
			data, reports := council.Folders(dir, settings)
			r, err := client.NewRelayForFile(cfg, configPath)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if client.NeedsRelayInfo(cfg) {
				learnRelayKeyWithin(ctx, r)
			}
			// The relay, not the config file, says who this machine is.
			me, err := r.WhoAmI(ctx)
			if err != nil {
				return fmt.Errorf("council serve: could not confirm this agent's identity with the relay: %w", client.RejoinHint(err, cfg.Relay))
			}
			if me.Name != council.DefaultAgentName {
				return wrongServeAgent("council serve", council.DefaultAgentName, "the relay knows the machine using "+configPath+" as", me.Name)
			}
			st, err := council.OpenStore(council.StorePathIn(data))
			if err != nil {
				return err
			}
			defer st.Close()
			svc, err := council.NewService(council.ServiceConfig{Relay: r, Config: settings, Store: st, ReportDir: reports, Log: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			cmd.PrintErrf("tincan council: serving as %s on %s, folder %s\n", me.Name, cfg.Relay, data)
			err = svc.Run(ctx)
			if ctx.Err() != nil {
				cmd.PrintErrln("tincan council: stopped")
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Council's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/council.json)")
	cmd.Flags().StringVar(&dir, "dir", "", councilDirFlagHelp)
	return cmd
}

func councilInstallCmd() *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the Council service definition (launchd on macOS, systemd on Linux); does not start it",
		Long: "Writes a launchd agent (" + council.ServiceLabel + ") on macOS, or a systemd user unit on Linux, that runs\n" +
			"tincan council serve with TINCAN_CONFIG=~/.config/tincan/council.json and keeps it running. It prints the\n" +
			"command that starts the service and never starts it itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := council.InstallService(history.ServiceOptions{Binary: binary})
			if err != nil {
				return err
			}
			cmd.Printf("council service definition: %s (not started)\n", res.Path)
			home, _ := os.UserHomeDir()
			cfgPath := filepath.Join(home, ".config", "tincan", "council.json")
			if cfg, err := client.LoadConfigFrom(cfgPath); err != nil || cfg.Relay == "" {
				cmd.Printf("Council is not joined yet; first invite it with --kind council, then run:\n  TINCAN_CONFIG=%s tincan join <code> --relay <relay url>\n", cfgPath)
			}
			cmd.Printf("start it with:\n  %s\n", res.Next)
			cmd.Println("then check it with: tincan council doctor")
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "binary", "", "tincan binary the service runs (default: this executable)")
	return cmd
}

func councilDoctorCmd() *cobra.Command {
	var configPath, dir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check Council's setup: its relay join and kind, council.json, and its folders",
		Long: "Checks that the relay knows this machine as the council agent with kind council (without it the\n" +
			"relay would not hold agents' councils for the owner, and council serve refuses to run), that\n" +
			"council.json is valid, and that Council's folders are writable. Exit status is 1 when a check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = serviceConfigPath(council.DefaultAgentName)
			}
			exe, err := os.Executable()
			if err == nil {
				exe, _ = filepath.EvalSymlinks(exe)
			}
			rep := runCouncilDoctor(cmd.Context(), exe, expandHome(configPath), councilDir(dir))
			if asJSON {
				if err := printJSON(cmd, rep); err != nil {
					return err
				}
			} else {
				printDoctor(cmd.OutOrStdout(), rep)
			}
			if !rep.OK {
				return errCouncilDoctorFailed
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Council's client config (default: $TINCAN_CONFIG, else ~/.config/tincan/council.json)")
	cmd.Flags().StringVar(&dir, "dir", "", councilDirFlagHelp)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

var errCouncilDoctorFailed = errors.New("tincan council doctor found problems (see above)")

func runCouncilDoctor(ctx context.Context, exe, configPath, dir string) doctorReport {
	rep := doctorReport{Version: Version, Binary: exe}
	rep.Checks = append(rep.Checks, councilRelayChecks(ctx, configPath)...)
	settingsPath := council.ConfigPathIn(dir)
	settings, err := council.LoadConfig(settingsPath)
	if err != nil {
		rep.Checks = append(rep.Checks, check{"council.json", "fail", err.Error(), "Fix or remove " + settingsPath + "; without it Council uses its defaults."})
	} else {
		detail := settingsPath + " is valid"
		if _, err := os.Stat(settingsPath); err != nil {
			detail = "no " + settingsPath + ", using the defaults"
		}
		rep.Checks = append(rep.Checks, check{"council.json", "ok", detail, ""})
		data, reports := council.Folders(dir, settings)
		rep.Checks = append(rep.Checks, writableCheck("data folder", data), writableCheck("report folder", reports))
	}
	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			rep.OK = false
		}
	}
	return rep
}

// writableCheck checks that dir exists or can be created owner-only, and
// that this user can write in it.
func writableCheck(name, dir string) check {
	fix := "Make " + dir + " writable by this user, or point council.json's dir or report_dir elsewhere."
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return check{name, "fail", "cannot create " + dir + ": " + err.Error(), fix}
	}
	f, err := os.CreateTemp(dir, ".doctor-")
	if err != nil {
		return check{name, "fail", dir + " is not writable: " + err.Error(), fix}
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return check{name, "ok", dir + " is writable", ""}
}

// councilRelayChecks checks Council's join and the kind the relay stores
// for it. Without kind council the relay does not hold agents' councils
// for the owner, and council serve refuses to run.
func councilRelayChecks(ctx context.Context, configPath string) []check {
	joinFix := "Join Council with its own config: TINCAN_CONFIG=~/.config/tincan/council.json tincan join <code> --relay <relay url>, or pass --config."
	cfg, err := client.LoadConfigFrom(configPath)
	if err != nil {
		return []check{{"relay", "fail", fmt.Sprintf("cannot read %s: %v", configPath, err), joinFix}}
	}
	if cfg.Relay == "" {
		return []check{{"relay", "fail", "no relay configured in " + configPath, joinFix}}
	}
	r, err := client.NewRelayForFile(cfg, configPath)
	if err != nil {
		return []check{{"relay", "fail", err.Error(), "Check the relay URL and proxy in " + configPath + "."}}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := r.WhoAmI(cctx)
	switch {
	case client.IsNotJoined(err):
		return []check{{"relay", "fail", "the relay does not know this machine as the council agent: " + err.Error(), "Run TINCAN_CONFIG=" + configPath + " tincan rejoin; a machine that was never joined needs an invite."}}
	case err != nil:
		return []check{{"relay", "fail", "cannot reach the relay at " + cfg.Relay + ": " + err.Error(), "Check that this machine is on the tailnet (tailscale status) and the relay is running."}}
	case me.Name != council.DefaultAgentName:
		return []check{{"relay", "fail", fmt.Sprintf("%s is joined as %q, not %q", configPath, me.Name, council.DefaultAgentName), joinFix}}
	}
	out := []check{{"relay", "ok", fmt.Sprintf("reachable at %s; this machine is agent %q", r.Base(), me.Name), ""}}
	kindFix := "Upgrade the relay to this release first, then on an admin device run: tincan kind council council. Until then council serve refuses to run."
	switch me.Kind {
	case onboard.KindCouncil:
		out = append(out, check{"relay kind", "ok", `the relay stores kind "council", so it holds agents' councils for the owner's approval`, ""})
	case "":
		out = append(out, check{"relay kind", "fail", "the relay stores no kind for council, so it would not hold agents' councils for the owner's approval", kindFix})
	default:
		out = append(out, check{"relay kind", "fail", fmt.Sprintf("the relay stores kind %q for council, not \"council\", so it would not hold agents' councils for the owner's approval", me.Kind), kindFix})
	}
	return out
}
