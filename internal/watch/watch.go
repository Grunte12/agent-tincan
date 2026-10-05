// Package watch is the relay watchdog behind tincan relay-watch: it probes
// the relay on a schedule and runs an alert command, which does not go
// through the relay, when the relay has been unreachable for a while and
// again when it answers once more.
package watch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// Kind is what an alert reports.
type Kind string

const (
	// Down is the alert for a relay unreachable for Watcher.After.
	Down Kind = "down"
	// Up is the alert for a relay that answers again after a Down alert.
	Up Kind = "up"
)

// Event is one alert: what happened, to which relay, since when, and the
// text to send the owner.
type Event struct {
	Kind    Kind
	Relay   string
	Since   time.Time // when the outage began (first failed probe)
	Message string
}

// Watcher turns probe results into at most one Down alert per outage and
// one Up alert when it ends. Its state is in memory only: a watcher
// restarted mid-outage starts counting again and may alert once more.
type Watcher struct {
	// Relay names the relay in alerts (its current URL).
	Relay func() string
	// After is how long probes must keep failing before the Down alert.
	After time.Duration
	// Probe reports whether the relay answers; nil means it does.
	Probe func(context.Context) error
	// Alert sends an event to the owner. An error is logged and the
	// alert is tried again at the next probe.
	Alert func(context.Context, Event) error
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logf logs (default log.Printf).
	Logf func(format string, args ...any)

	downSince time.Time // first failed probe of the current outage; zero when up
	lastErr   error     // the latest failed probe's error
	alerted   bool      // the Down alert for this outage was delivered
	pendingUp *Event    // a recovery alert that failed, retried before each probe
}

// Run probes now and then every interval until ctx ends, and returns
// ctx's error.
func (w *Watcher) Run(ctx context.Context, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.Check(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Check runs one probe and sends whatever alert it calls for.
func (w *Watcher) Check(ctx context.Context) {
	if w.pendingUp != nil && w.send(ctx, *w.pendingUp) {
		w.pendingUp = nil
	}
	now := w.now()
	if err := w.Probe(ctx); err != nil {
		w.lastErr = err
		if w.downSince.IsZero() {
			w.downSince = now
			w.logf("relay-watch: %s unreachable: %v", w.relay(), err)
		}
		if !w.alerted && now.Sub(w.downSince) >= w.After {
			msg := fmt.Sprintf("Tincan relay %s has been unreachable for %s (since %s): %v",
				w.relay(), span(now.Sub(w.downSince)), w.downSince.Local().Format("Jan 2 15:04 MST"), err)
			w.alerted = w.send(ctx, Event{Kind: Down, Relay: w.relay(), Since: w.downSince, Message: msg})
		}
		return
	}
	if w.downSince.IsZero() {
		return
	}
	if !w.alerted {
		// Back before anyone was told: nothing to recover from.
		w.logf("relay-watch: %s answers again after %s", w.relay(), span(now.Sub(w.downSince)))
		w.downSince = time.Time{}
		return
	}
	msg := fmt.Sprintf("Tincan relay %s was back at %s after %s (down since %s).",
		w.relay(), now.Local().Format("Jan 2 15:04 MST"), span(now.Sub(w.downSince)), w.downSince.Local().Format("Jan 2 15:04 MST"))
	up := Event{Kind: Up, Relay: w.relay(), Since: w.downSince, Message: msg}
	// The outage is over either way, so a new one is tracked and alerted
	// on its own; a recovery alert that failed is retried before each probe.
	w.downSince, w.alerted = time.Time{}, false
	if !w.send(ctx, up) {
		w.pendingUp = &up
	}
}

// send runs the alert and reports whether it was delivered.
func (w *Watcher) send(ctx context.Context, e Event) bool {
	if err := w.Alert(ctx, e); err != nil {
		w.logf("relay-watch: %s alert failed (will retry at the next probe): %v", e.Kind, err)
		return false
	}
	w.logf("relay-watch: sent %s alert: %s", e.Kind, e.Message)
	return true
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) relay() string {
	if w.Relay != nil {
		return w.Relay()
	}
	return "relay"
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// span is d to the minute, without the trailing "0s" ("25m", "1h5m").
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}
	s := strings.TrimSuffix(d.String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m") // a whole hour reads "1h", not "1h0m"
	}
	return s
}

// alertTimeout bounds one run of the alert command, so a command that
// hangs (osascript waiting on a permission prompt) does not stop the
// watcher from probing and retrying.
const alertTimeout = time.Minute

// Command returns an Alert that runs cmdline with /bin/sh -c, passing the
// event in TINCAN_WATCH_EVENT (down or up), TINCAN_WATCH_RELAY,
// TINCAN_WATCH_SINCE (RFC 3339, UTC) and TINCAN_WATCH_MESSAGE. A non-zero
// exit is an error that carries the command's output.
func Command(cmdline string) func(context.Context, Event) error {
	return func(ctx context.Context, e Event) error {
		ctx, cancel := context.WithTimeout(ctx, alertTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
		cmd.Env = append(os.Environ(),
			"TINCAN_WATCH_EVENT="+string(e.Kind),
			"TINCAN_WATCH_RELAY="+e.Relay,
			"TINCAN_WATCH_SINCE="+e.Since.UTC().Format(time.RFC3339),
			"TINCAN_WATCH_MESSAGE="+e.Message,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if o := strings.TrimSpace(string(bytes.ToValidUTF8(out, nil))); o != "" {
				if len(o) > 500 {
					o = o[:500] + "..."
				}
				return fmt.Errorf("alert command: %w: %s", err, o)
			}
			return fmt.Errorf("alert command: %w", err)
		}
		return nil
	}
}

// ServiceLabel is the launchd label of the relay watchdog service.
const ServiceLabel = "com.agenttincan.relaywatch"

// systemdUnit is the systemd user unit name of the relay watchdog service.
const systemdUnit = "tincan-relay-watch.service"

// launchdTemplate is the watchdog's launchd agent. Placeholders:
// __TINCAN_BINARY__, __HOME__, __PATH__, __CONFIG__, __ALERT__, __AFTER__,
// __EVERY__, each XML-escaped when filled.
const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan relay watchdog: runs "tincan relay-watch" under launchd.
  "tincan relay-watch install" writes this file into ~/Library/LaunchAgents
  with the real values filled in. It does not load it; start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.relaywatch.plist
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.relaywatch</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>relay-watch</string>
    <string>--config</string>
    <string>__CONFIG__</string>
    <string>--alert-cmd</string>
    <string>__ALERT__</string>
    <string>--after</string>
    <string>__AFTER__</string>
    <string>--every</string>
    <string>__EVERY__</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>__PATH__</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>__HOME__/Library/Logs/tincan-relay-watch.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-relay-watch.log</string>
</dict>
</plist>
`

// systemdTemplate is the Linux user unit.
const systemdTemplate = `[Unit]
Description=Agent Tincan relay watchdog
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" relay-watch --config "__CONFIG__" --alert-cmd "__ALERT__" --after __AFTER__ --every __EVERY__
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// ServiceOptions controls InstallService: the client config the watchdog
// probes with (an absolute path), the alert command, and the timings.
type ServiceOptions struct {
	history.ServiceOptions
	Config   string
	AlertCmd string
	After    time.Duration
	Every    time.Duration
}

// InstallService writes the relay watchdog's service definition (a launchd
// agent on macOS, a systemd user unit on Linux) that runs tincan
// relay-watch with o's config, alert command and timings. It never loads
// or starts it.
func InstallService(o ServiceOptions) (history.ServiceResult, error) {
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	switch {
	case o.AlertCmd == "":
		return history.ServiceResult{}, errors.New("no alert command given")
	case strings.ContainsAny(o.AlertCmd, "\n\r\x00"):
		return history.ServiceResult{}, errors.New("the alert command must be one line")
	case !filepath.IsAbs(o.Config) || strings.ContainsAny(o.Config, "\n\r\x00\"$%"):
		return history.ServiceResult{}, fmt.Errorf("config %q must be an absolute path without quotes, $, %% or newlines", o.Config)
	case o.After <= 0 || o.Every <= 0:
		return history.ServiceResult{}, errors.New("after and every must be positive")
	}
	alert := o.AlertCmd
	if goos == "linux" {
		// systemd expands $ in ExecStart; $$ is a literal $. (The shared
		// escaping already handles quotes, backslashes and %.)
		alert = strings.ReplaceAll(alert, "$", "$$")
	}
	return history.InstallServiceDef(o.ServiceOptions, history.ServiceDef{
		Label:   ServiceLabel,
		Unit:    systemdUnit,
		Launchd: launchdTemplate,
		Systemd: systemdTemplate,
		Vars: []string{
			"__CONFIG__", o.Config,
			"__ALERT__", alert,
			"__AFTER__", o.After.String(),
			"__EVERY__", o.Every.String(),
		},
		Unsupported: "no relay watchdog service definition for " + goos + "; run tincan relay-watch under your own service manager",
	})
}
