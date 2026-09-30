package council

import (
	"runtime"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// ServiceLabel is the launchd label of the Council service.
const ServiceLabel = "com.agenttincan.council"

// systemdUnit is the systemd user unit name of the Council service.
const systemdUnit = "tincan-council.service"

// launchdTemplate is the Council service's launchd agent. Placeholders:
// __TINCAN_BINARY__, __HOME__, __PATH__, each XML-escaped when filled.
const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!--
  Agent Tincan Council: runs "tincan council serve" under launchd.
  "tincan council install" writes this file into ~/Library/LaunchAgents with
  the real paths filled in. It does not load it; start it with:
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.agenttincan.council.plist
  See docs/adapters/council.md.
-->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.agenttincan.council</string>
  <key>ProgramArguments</key>
  <array>
    <string>__TINCAN_BINARY__</string>
    <string>council</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>TINCAN_CONFIG</key>
    <string>__HOME__/.config/tincan/council.json</string>
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
  <string>__HOME__/Library/Logs/tincan-council.log</string>
  <key>StandardErrorPath</key>
  <string>__HOME__/Library/Logs/tincan-council.log</string>
</dict>
</plist>
`

// systemdTemplate is the Linux user unit. %h is systemd's home specifier.
const systemdTemplate = `[Unit]
Description=Agent Tincan Council
After=network-online.target

[Service]
ExecStart="__TINCAN_BINARY__" council serve
Environment=TINCAN_CONFIG=%h/.config/tincan/council.json
Environment="PATH=__PATH__"
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`

// InstallService writes the Council service definition, which runs
// tincan council serve as the agent in ~/.config/tincan/council.json. It
// never loads or starts it.
func InstallService(o history.ServiceOptions) (history.ServiceResult, error) {
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	return history.InstallServiceDef(o, history.ServiceDef{
		Label:       ServiceLabel,
		Unit:        systemdUnit,
		Launchd:     launchdTemplate,
		Systemd:     systemdTemplate,
		Unsupported: "no council service definition for " + goos + "; run tincan council serve under your own service manager",
	})
}
