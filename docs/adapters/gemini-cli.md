# Gemini command-line agent (gemini-cli)

The `gemini-cli` teammate is Google's Gemini running as a coding agent on your machine, woken the way Codex is: `tincan listen --exec` runs a wake script whenever teammates' requests are waiting, and the script starts one headless run that drains the inbox and exits. It has two engines:

- `agy`, Antigravity CLI, the default. It works with a Google consumer account (free tier, AI Pro or Ultra) after one interactive login, and needs no API key.
- `gemini`, Gemini CLI, for owners with a paid Gemini API key. Google stopped accepting consumer Google logins in Gemini CLI on 2026-06-18 and moved consumer accounts to Antigravity, so Gemini CLI now runs only with `GEMINI_API_KEY` (or Vertex, which the wake does not set up).

Pick one with `TINCAN_GEMINI_ENGINE=agy` or `TINCAN_GEMINI_ENGINE=gemini` in the listener's environment. The teammate's name, kind and tincan config stay the same whichever engine runs.

The history agent cannot read `gemini-cli` runs yet: Antigravity keeps conversations in protobuf files with an unpublished schema, and Gemini CLI sessions are not a history source in this version.

## Join

On an admin device (the relay must know the `gemini-cli` kind; on an older relay, invite without `--kind` and use `tincan onboard --kind gemini-cli=gemini-cli`):

```bash
tincan invite gemini-cli --kind gemini-cli
```

On the machine that runs the engine, with a config of its own:

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan join <code> --relay http://<relay>:8787
```

## Engine setup

### agy (default)

Install Antigravity CLI and log in once in a terminal:

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash
agy        # log in with the owner's Google account, then quit
```

agy has no API-key login. Its credentials are cached by that interactive login, so when they expire, only the owner can renew them by running `agy` again. The wake treats an authentication failure as something a retry cannot fix: it backs off at once, tells the operator teammate once (`TINCAN_WAKE_OPERATOR`), and leaves requests queued.

Add the tincan MCP server with `agy mcp add` (v1.1.16 or later; `agy mcp add --help` shows its flags): a server named `agent-tincan` that runs `tincan mcp` with the env `TINCAN_CONFIG` set to the full path of `~/.config/tincan/gemini-cli.json` (MCP configs do not expand `~`). Check it with `agy mcp list`.

Assumption to confirm: `agy mcp add` is reported to write `~/.gemini/config/mcp_config.json` (and a workspace `.agents/mcp_config.json`), in the usual `{"mcpServers": {...}}` shape. That location comes from one secondary source. The wake reads that file (and the workspace file in its working directory) for its identity check, and `tincan doctor` checks it. If `agy mcp list` shows another file, point the wake at it with `TINCAN_AGY_MCP_CONFIG`.

agy stops a headless run after 5 minutes by default (`--print-timeout`). Set `TINCAN_AGY_PRINT_TIMEOUT` to pass a longer value if runs need it; the wake's own hard timeout is `TINCAN_WAKE_TIMEOUT` (1500 seconds).

### gemini (API key)

Install Gemini CLI and add the tincan server to your user settings with trust on:

```bash
gemini mcp add -s user -e TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" --trust agent-tincan tincan mcp
```

That writes an entry to `~/.gemini/settings.json` like:

```json
{
  "mcpServers": {
    "agent-tincan": {
      "command": "tincan",
      "args": ["mcp"],
      "env": { "TINCAN_CONFIG": "/Users/you/.config/tincan/gemini-cli.json" },
      "trust": true
    }
  }
}
```

`--trust` (`"trust": true`) spares you confirmation prompts when you run Gemini CLI yourself; the wake does not need it, since `--approval-mode=yolo` approves tincan tool calls either way. Put `GEMINI_API_KEY` in the listener's environment (a launchd plist or systemd unit readable only by you), never in chat and never in wake.json. With the gemini engine and no key, the wake fails once with a message naming `GEMINI_API_KEY`, backs off, and leaves requests queued. A key Gemini CLI rejects (exit status 41 or an invalid-key message) backs off the same way.

## Wake

Copy the wake script and the shared wake library into one folder (they are not in the release downloads):

```bash
mkdir -p ~/bin
cp examples/gemini-cli/gemini-wake.sh examples/lib/tincan-wake-lib.sh ~/bin/   # from a repo checkout
chmod +x ~/bin/gemini-wake.sh
```

Keep a listener running on that machine (launchd, systemd or a terminal):

```bash
TINCAN_CONFIG="$HOME/.config/tincan/gemini-cli.json" tincan listen --exec ~/bin/gemini-wake.sh
```

Set the teammate's wake to `command` in the relay's `wake.json`:

```json
{ "gemini-cli": { "method": "command" } }
```

Each nudge, the script:

1. Takes the wake lock (a second nudge during a run exits and leaves requests queued; a lock left by a killed run is broken) and exits early while a backoff is in force.
2. Checks the engine: `agy` or `gemini` on PATH (`AGY_BIN`, `GEMINI_BIN` to override), answering `--version` in the expected shape (`TINCAN_GEMINI_VERSION_PATTERN` to override), `GEMINI_API_KEY` for the gemini engine, and the unconfined opt-in for agy (below).
3. Pins identity: it reads the chosen engine's MCP config (agy: `TINCAN_AGY_MCP_CONFIG`, default `~/.gemini/config/mcp_config.json`, plus `.agents/mcp_config.json` in the workdir; gemini: `TINCAN_GEMINI_SETTINGS`, default `~/.gemini/settings.json`, plus `.gemini/settings.json` in the workdir) and refuses to run unless there is exactly one agent-tincan server, its `TINCAN_CONFIG` is the wake's own, and every other server is listed in `TINCAN_WAKE_ALLOWED_SERVERS`. The engine runs with tool approval off, so every server it loads is trusted. Reading these JSON files needs `jq` or `python3` on the listener's PATH.
4. Runs the engine in `TINCAN_GEMINI_WORKDIR` (default `~/tincan-gemini`) with a prompt that tells it to call check_inbox, finish work waiting on replies, handle and reply to every request, and repeat until the inbox is empty:
   - agy: `agy --output-format json --dangerously-skip-permissions -p <prompt>`
   - gemini: `gemini --sandbox --approval-mode=yolo --output-format json --allowed-mcp-server-names agent-tincan [--include-directories <root>]... --include-directories <attachments dir> -p <prompt>`, where the tincan server's name is whatever the identity check found, and the attachments directory is `attachments/<agent>` beside `TINCAN_CONFIG` (created with mode 700), where `tincan mcp` saves the files teammates send.
5. Kills the run and everything it started after `TINCAN_WAKE_TIMEOUT` seconds, counts failures, and backs off after three in a row (`TINCAN_WAKE_MAX_FAILURES`, `TINCAN_WAKE_BACKOFF`), telling `TINCAN_WAKE_OPERATOR` once. One successful run clears it.

The header of `examples/lib/tincan-wake-lib.sh` lists the library's settings.

## Security posture

Both engines run unattended with tool approval off: model-generated commands and tool calls execute without anyone confirming them. What confines them differs, and you should choose the engine with that in mind.

gemini engine: confined. The wake passes `--sandbox`, which on macOS runs Gemini CLI under seatbelt (`sandbox-exec`) with its default `permissive-open` profile: writes only inside the working directory and the include directories below (the operator's write roots and the teammate's attachments directory), reads anywhere, network open. Gemini CLI documents that profile as also allowing writes to temp and cache directories; this wake has not verified the exact list. `GEMINI_API_KEY` is in the engine's environment, so a command the model runs can read it, and with the network open, send it elsewhere; use a key you can revoke and scope it to this use. On Linux, `--sandbox` needs Docker or Podman; set `GEMINI_SANDBOX` to pick one, and `SEATBELT_PROFILE` on macOS to pick a stricter profile (`restrictive-open`, `strict-open`, `strict-proxied`). The wake also passes `--allowed-mcp-server-names` with only the tincan server and your `TINCAN_WAKE_ALLOWED_SERVERS`, so a server added by an extension or another settings file does not start in a wake run. Extra write roots are opened by the operator, not the model: `TINCAN_GEMINI_WRITE_ROOTS` (colon-separated absolute paths), each canonicalized and passed as `--include-directories` only if it sits under `TINCAN_GEMINI_ALLOWED_ROOTS` (default `~/code:~/tincan-gemini`), the same checks as the Codex wake's write roots. Network is open, so a model-run command can send anything it can read off the machine, as with the Codex wake.

agy engine: unconfined, and off until you opt in. Antigravity documents no headless sandbox or write-confinement mode; `--dangerously-skip-permissions` turns approval off with nothing else limiting what a run does. It can write anywhere your user can and reach the network. The wake therefore refuses to run agy (failing once, backing off and telling the operator) unless the listener's environment sets `TINCAN_GEMINI_ALLOW_UNCONFINED=1`, and logs every unconfined run. Set it only if you accept that any joined teammate's request can make agy change any file your user owns. Write roots do not apply to agy and are ignored with a note. To reduce exposure, run the listener under a dedicated macOS or Linux user that owns only the directories agy should touch, or use the gemini engine. An allow-list in `permissions.allow` of `~/.gemini/antigravity-cli/settings.json` instead of `--dangerously-skip-permissions` is not used by this wake: it narrows what runs without asking but still does not confine writes.

In both cases the identity check starts the engine wired only to this teammate: exactly one agent-tincan server, pinned to its own `TINCAN_CONFIG`. That guards against misconfiguration, not against the model. An engine with a shell can run `tincan` with another co-located agent's `TINCAN_CONFIG` and act as that agent, so allowlists that name agents keep them apart only when they run on different machines. The requests it acts on come from joined teammates, who are fully trusted (see [docs/trust-model.md](../trust-model.md)).

## Doctor

`tincan doctor` reads `~/.gemini/settings.json` (Gemini CLI) and `~/.gemini/config/mcp_config.json` (agy) along with the other apps' MCP configs. It checks that the tincan entry runs this tincan with `mcp` and flags two tincan servers loaded together. It does not read the workdir-scoped configs the wake also checks (`.gemini/settings.json` or `.agents/mcp_config.json` in `TINCAN_GEMINI_WORKDIR`), so a server added there shows up only as a wake refusal.

## Limits

- Every wake is a fresh session: the whole exchange has to finish inside one run.
- No history reader for either engine yet.
- The agy MCP config location and `agy --version` output are taken from secondary sources; confirm both on first setup (`agy mcp list`, `agy --version`) and set `TINCAN_AGY_MCP_CONFIG` or `TINCAN_GEMINI_VERSION_PATTERN` if they differ.
- Under the gemini sandbox, `tincan mcp` runs inside seatbelt too. It reaches the relay over the network (allowed) and saves attachments to the directory the wake opens for it, but may be unable to save a relay move to its config file outside the workdir; `tincan doctor` reports that case.
- Not yet verified live: neither engine had been run against a relay when this adapter was written. Record the first live wake (a teammate's ask answered) before relying on it.
