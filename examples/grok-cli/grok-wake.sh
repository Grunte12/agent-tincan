#!/bin/sh
# Wake for the grok-cli teammate: xAI's Grok Build CLI (grok), run headless
# by "tincan listen --exec".
#
# Grok Build has no daemon, so an idle grok-cli needs something to start a
# fresh run when teammates' requests are waiting. Copy this script and
# examples/lib/tincan-wake-lib.sh from the agent-tincan repo (they are not in
# the release downloads) into one folder you keep, chmod +x this one, and run:
#
#   TINCAN_CONFIG=~/.config/tincan/grok-cli.json tincan listen --exec ~/bin/grok-wake.sh
#
# Grok runs in a wake home of its own: HOME is the wake home and GROK_HOME
# its .grok folder. Grok Build also loads MCP servers from ~/.claude.json,
# ~/.cursor/mcp.json and Claude plugins, and with approval off every one of
# them would be trusted, so the wake keeps the owner's home out of reach of
# that discovery. The wake home's .grok/config.toml holds only this
# teammate's [mcp_servers.agent-tincan] entry, and its login
# (GROK_HOME=<wake home>/.grok grok login, or XAI_API_KEY in the listener's
# environment). See docs/adapters/grok-cli.md.
#
# Each run is a new session whose id the wake picks up front and records,
# with the workdir, in a private list beside TINCAN_CONFIG
# (<name>.wake-sessions), so the history agent leaves wake runs out of
# "what did I ask Grok CLI".
#
# Settings, read from the listener's environment (the library's settings,
# TINCAN_WAKE_*, are listed in tincan-wake-lib.sh):
#
#   TINCAN_CONFIG                this teammate's tincan config (default
#                                ~/.config/tincan/grok-cli.json)
#   TINCAN_GROK_WAKE_HOME        the wake home (default <name>.wake beside
#                                TINCAN_CONFIG, ~/.config/tincan/grok-cli.wake)
#   TINCAN_GROK_WORKDIR          where grok runs (default <wake home>/work)
#   TINCAN_GROK_WRITE_ROOTS      extra write roots, colon-separated absolute
#                                paths
#   TINCAN_GROK_ALLOWED_ROOTS    where write roots may be (default
#                                ~/code:<workdir>)
#   TINCAN_GROK_VERSION_PATTERN  what "grok --version" must print (default
#                                Grok Build's "grok X.Y.Z (<commit>)")
#   TINCAN_GROK_JSON_TOOL        jq or python3, to read "grok inspect --json"
#                                (default jq when on PATH)
#   XAI_API_KEY                  an xAI API key, instead of a login in the
#                                wake home
#   GROK_BIN                     the grok binary (default grok)
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/grok-cli.json}"
# shellcheck disable=SC2088 # a literal ~/ prefix, which tincan expands
case $TINCAN_CONFIG in "~/"*) TINCAN_CONFIG=$HOME/${TINCAN_CONFIG#"~/"} ;; esac
export TINCAN_CONFIG

# The library sits next to this script, or in the repo's examples/lib.
here=$(dirname -- "$0")
if [ -z "${TINCAN_WAKE_LIB:-}" ]; then
  TINCAN_WAKE_LIB=$here/tincan-wake-lib.sh
  [ -f "$TINCAN_WAKE_LIB" ] || TINCAN_WAKE_LIB=$here/../lib/tincan-wake-lib.sh
fi
if [ ! -f "$TINCAN_WAKE_LIB" ]; then
  echo "grok-cli-wake: tincan-wake-lib.sh not found; copy examples/lib/tincan-wake-lib.sh next to this script" >&2
  exit 2
fi
# shellcheck source=SCRIPTDIR/../lib/tincan-wake-lib.sh
. "$TINCAN_WAKE_LIB"

# The sandbox profile the wake writes and runs with. The history agent
# knows this name too (history.GrokWakeSandboxProfile).
PROFILE=tincan-wake
cfg_dir=$(dirname -- "$TINCAN_CONFIG")
cfg_name=$(basename -- "$TINCAN_CONFIG" .json)
OWNER_HOME=$HOME
WAKE_HOME=${TINCAN_GROK_WAKE_HOME:-$cfg_dir/$cfg_name.wake}
WAKE_HOME=${WAKE_HOME%/}
GROK_HOME_DIR=$WAKE_HOME/.grok
WORKDIR=${TINCAN_GROK_WORKDIR:-$WAKE_HOME/work}
SESSIONS=$cfg_dir/$cfg_name.wake-sessions
ALLOWED_ROOTS=${TINCAN_GROK_ALLOWED_ROOTS:-$HOME/code:$WORKDIR}
WRITE_ROOTS=${TINCAN_GROK_WRITE_ROOTS:-}
BIN=${GROK_BIN:-grok}
# Grok Build prints "grok 1.0.40 (eb1a2256660d) [stable]"; the community
# grok-cli prints a bare version number.
VERSION_PATTERN=${TINCAN_GROK_VERSION_PATTERN:-^grok [0-9]+[.][0-9]+[.][0-9]+ [(]}

tincan_wake_init grok-cli
tincan_wake_begin

tincan_wake_check_binary "$BIN" "$VERSION_PATTERN"

# The wake home must not be the owner's home, where Grok would import the
# owner's Claude Code and Cursor MCP servers along with this teammate's.
if ! mkdir -p "$WAKE_HOME" 2>/dev/null || ! wake_home_c=$(tincan_wake_canonical_dir "$WAKE_HOME"); then
  tincan_wake_refuse "cannot create the wake home $WAKE_HOME (set TINCAN_GROK_WAKE_HOME to an absolute path this user can write)"
fi
if [ "$wake_home_c" = "$(tincan_wake_canonical_dir "$OWNER_HOME" || echo "$OWNER_HOME")" ]; then
  tincan_wake_refuse "the wake home $WAKE_HOME is the owner's home, where Grok would load the owner's other MCP servers; set TINCAN_GROK_WAKE_HOME to a directory of its own"
fi
if ! { chmod 700 "$WAKE_HOME" && mkdir -p "$GROK_HOME_DIR" "$WORKDIR"; } 2>/dev/null || ! workdir_c=$(tincan_wake_canonical_dir "$WORKDIR"); then
  tincan_wake_refuse "cannot create $GROK_HOME_DIR or the workdir $WORKDIR"
fi

# During a run ~ is the wake home, so a tincan command the model runs with
# TINCAN_CONFIG=~/.config/tincan/<name>.json (as the standing instructions
# write it) would miss the config. When the config sits under the owner's
# home, the same relative path in the wake home links to its directory.
case $cfg_dir in
  "$OWNER_HOME"/*)
    link=$WAKE_HOME/${cfg_dir#"$OWNER_HOME"/}
    if [ ! -e "$link" ] && [ ! -L "$link" ]; then
      { mkdir -p "$(dirname -- "$link")" && ln -s "$cfg_dir" "$link"; } 2>/dev/null ||
        tincan_wake_log "cannot link $link to $cfg_dir; tincan commands that name the config under ~ will miss it during a run"
    fi
    ;;
esac

# grok_json_tool prints the JSON reader to use, jq or python3
# (TINCAN_GROK_JSON_TOOL, else jq when on PATH), or nothing when that tool
# is not on PATH.
grok_json_tool() {
  _tool=${TINCAN_GROK_JSON_TOOL:-}
  if [ -z "$_tool" ]; then
    if command -v jq >/dev/null 2>&1; then _tool=jq; else _tool=python3; fi
  fi
  case $_tool in
    jq | python3) command -v "$_tool" >/dev/null 2>&1 && echo "$_tool" ;;
  esac
  return 0
}
JSON_TOOL=$(grok_json_tool)
if [ -z "$JSON_TOOL" ]; then
  tincan_wake_refuse "reading grok inspect --json needs jq or python3 on the listener's PATH (or TINCAN_GROK_JSON_TOOL names one that is missing)"
fi

# A login lives in the wake home's auth.json; XAI_API_KEY is the other way.
if [ -z "${XAI_API_KEY:-}" ] && [ ! -s "$GROK_HOME_DIR/auth.json" ]; then
  tincan_wake_refuse "Grok Build is not logged in for this wake: run GROK_HOME=$GROK_HOME_DIR grok login once on this machine, or set XAI_API_KEY in the listener's environment"
fi

# grok_env runs a command with the wake home as HOME and GROK_HOME, and
# Grok's update check off (an update mid-run would change the binary the
# version check vetted).
# shellcheck disable=SC2329 # run through grok_mcp_list
grok_env() {
  HOME=$WAKE_HOME GROK_HOME=$GROK_HOME_DIR GROK_DISABLE_AUTOUPDATER=1 "$@"
}

INSPECT=$TINCAN_WAKE_STATE_DIR/inspect.json

# grok_inspect_rows prints one line per MCP server in the saved inspect
# output: name, source type, source path and target, separated by the
# ASCII unit separator (so an empty field stays in place for read).
# shellcheck disable=SC2329 # run through grok_mcp_list
grok_inspect_rows() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -r '(.mcpServers // [])[]
      | [.name // "", .source.type // "", .source.path // "", .target // ""]
      | map(tostring | gsub("[\t\n\u001f]"; " ")) | join("\u001f")' "$INSPECT"
  else
    python3 -c '
import json, sys
for s in json.load(open(sys.argv[1])).get("mcpServers") or []:
    src = s.get("source") or {}
    row = [s.get("name") or "", src.get("type") or "", src.get("path") or "", s.get("target") or ""]
    print("\x1f".join(str(x).replace("\t", " ").replace("\n", " ").replace("\x1f", " ") for x in row))
' "$INSPECT"
  fi
}

# grok_extras prints how many plugins and hooks the saved inspect output
# lists.
grok_extras() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -r '((.plugins // []) | length) + ((.hooks // []) | length)' "$INSPECT"
  else
    python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
print(len(d.get("plugins") or []) + len(d.get("hooks") or []))
' "$INSPECT"
  fi
}

# grok_toml_entry FILE NAME prints "TINCAN_CONFIG<TAB>command args" for the
# [mcp_servers.NAME] table of a Grok config.toml, reading its env either as
# an [mcp_servers.NAME.env] table or inline (env = { TINCAN_CONFIG = ... }).
# Only one-line values are read; anything it cannot read comes out empty,
# which the identity check refuses.
# shellcheck disable=SC2016 # awk program text, expanded by awk
_grok_toml_awk='
function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
function val(s,   q, i) {
  s = trim(s); q = substr(s, 1, 1)
  if (q != "\"" && q != "\047") return ""
  s = substr(s, 2); i = index(s, q)
  return i ? substr(s, 1, i - 1) : ""
}
function after(s) { return substr(s, index(s, "=") + 1) }
function arr(s,   out, n, parts, i, v) {
  s = after(s); sub(/^[ \t]*\[/, "", s); sub(/\][ \t]*(#.*)?$/, "", s)
  n = split(s, parts, ",")
  for (i = 1; i <= n; i++) { v = val(parts[i]); if (v != "") out = out " " v }
  return out
}
/^[ \t]*\[/ {
  h = $0; sub(/^[ \t]*\[[ \t]*/, "", h); sub(/[ \t]*\][ \t]*(#.*)?$/, "", h)
  gsub(/"/, "", h); gsub(/[ \t]/, "", h)
  sec = ""
  if (h == "mcp_servers." want) sec = "main"
  if (h == "mcp_servers." want ".env") sec = "env"
  next
}
sec == "main" && /^[ \t]*command[ \t]*=/ { cmd = val(after($0)) }
sec == "main" && /^[ \t]*args[ \t]*=/ { args = arr($0) }
sec == "main" && /^[ \t]*env[ \t]*=/ { s = $0; i = index(s, "TINCAN_CONFIG"); if (i) cfg = val(after(substr(s, i))) }
sec == "main" && /^[ \t]*env\."?TINCAN_CONFIG"?[ \t]*=/ { cfg = val(after($0)) }
sec == "env" && /^[ \t]*"?TINCAN_CONFIG"?[ \t]*=/ { cfg = val(after($0)) }
END { printf "%s\t%s%s\n", cfg, cmd, args }
'
# shellcheck disable=SC2329 # run through grok_mcp_list
grok_toml_entry() {
  awk -v want="$2" "$_grok_toml_awk" "$1"
}

# grok_mcp_list prints the MCP servers Grok discovers for the workdir, run
# the way the wake runs it, as the library's listing lines: name,
# TINCAN_CONFIG, command. Grok's listing does not show a server's env, so
# for a server from a config.toml it is read from that file's entry; any
# other source (a Claude or Cursor import, a plugin) has none.
# shellcheck disable=SC2329 # run by tincan_wake_check_identity
grok_mcp_list() {
  (cd "$workdir_c" && grok_env "$BIN" inspect --json) >"$INSPECT" || return 1
  _us=$(printf '\037')
  grok_inspect_rows | while IFS=$_us read -r _name _type _path _target; do
    [ -n "$_name$_type$_path$_target" ] || continue
    _entry=$(printf '\t%s' "$_target")
    case $_path in
      *.toml) [ -f "$_path" ] && _entry=$(grok_toml_entry "$_path" "$_name") ;;
    esac
    printf '%s\t%s\n' "$_name" "$_entry"
  done
}

tincan_wake_check_identity grok_mcp_list

# Plugins and hooks run with approval off too, and the identity check does
# not vet them.
extras=$(grok_extras 2>/dev/null) || extras=unknown
if [ "$extras" != 0 ]; then
  tincan_wake_refuse "grok lists plugins or hooks in the wake home ($GROK_HOME_DIR); they would run unattended, so remove them there (grok inspect --json with HOME=$WAKE_HOME GROK_HOME=$GROK_HOME_DIR shows them)"
fi
rm -f "$INSPECT"

# grok_attachment_dir prints where "tincan mcp" saves the attachments this
# teammate receives: attachments/<agent> beside TINCAN_CONFIG, with the
# agent name read from the config ("default" when it is missing or not a
# plain name, as tincan does).
grok_attachment_dir() {
  _agent=
  if [ -r "$TINCAN_CONFIG" ]; then
    case $JSON_TOOL in
      jq) _agent=$(jq -r '.agent // "" | tostring' "$TINCAN_CONFIG" 2>/dev/null) || _agent= ;;
      python3) _agent=$(python3 -c '
import json, sys
print(str(json.load(open(sys.argv[1])).get("agent") or ""))
' "$TINCAN_CONFIG" 2>/dev/null) || _agent= ;;
    esac
  fi
  if ! printf '%s\n' "$_agent" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$' || [ "$_agent" = . ] || [ "$_agent" = .. ]; then
    _agent=default
  fi
  printf '%s\n' "$cfg_dir/attachments/$_agent"
}

# The sandbox profile: Grok's workspace profile (read everywhere; write the
# workdir, temp directories and GROK_HOME, whose config, sandbox and hook
# files stay write-protected) plus the teammate's attachments directory,
# where tincan mcp saves received files, and the operator's write roots.
# A custom profile makes Grok refuse to start when it cannot apply it,
# where a built-in one would run unconfined with a warning. The file is
# rewritten before every run.
roots=
att=$(grok_attachment_dir)
if { mkdir -p "$att" && chmod 700 "$att"; } 2>/dev/null && attc=$(tincan_wake_canonical_dir "$att"); then
  roots=$attc
else
  tincan_wake_log "cannot create the attachments directory $att; received attachments will not be saved"
fi
extra=$(tincan_wake_write_roots "$ALLOWED_ROOTS" "$WRITE_ROOTS")
if [ -n "$extra" ]; then
  roots="${roots:+$roots
}$extra"
fi

toml_string() {
  printf '"%s"' "$(printf '%s' "$1" | sed 's/[\\"]/\\&/g')"
}
sandbox=$GROK_HOME_DIR/sandbox.toml
if ! {
  echo "# Written by grok-wake.sh before every run; changes here are replaced."
  echo "[profiles.$PROFILE]"
  echo 'extends = "workspace"'
  printf 'read_write = ['
  sep=
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    printf '%s%s' "$sep" "$(toml_string "$d")"
    sep=', '
  done <<EOF
$roots
EOF
  echo ']'
} >"$sandbox.$$" || ! mv -f "$sandbox.$$" "$sandbox"; then
  rm -f "$sandbox.$$"
  tincan_wake_refuse "cannot write $sandbox"
fi

# The session id is chosen up front and recorded before the run, so even a
# run the timeout kills is known to the history agent.
grok_uuid() {
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr 'A-F' 'a-f'
  elif [ -r /proc/sys/kernel/random/uuid ]; then
    cat /proc/sys/kernel/random/uuid
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import uuid; print(uuid.uuid4())'
  fi
}
SID=$(grok_uuid) || SID=
if ! printf '%s\n' "$SID" | grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'; then
  tincan_wake_refuse "cannot make a session id (needs uuidgen, /proc/sys/kernel/random/uuid or python3)"
fi
if (umask 077 && touch "$SESSIONS") 2>/dev/null && chmod 600 "$SESSIONS" 2>/dev/null; then
  grep -qxF "workdir $workdir_c" "$SESSIONS" || printf 'workdir %s\n' "$workdir_c" >>"$SESSIONS"
  printf '%s\n' "$SID" >>"$SESSIONS"
else
  tincan_wake_log "cannot record the session id in $SESSIONS; the history agent will still leave this run out by its workdir"
fi

PROMPT="You have ${TINCAN_WAITING:-some} Agent Tincan item(s) waiting: requests from teammates, or replies to requests you sent. Call check_inbox. It shows replies to your requests first, with what you asked: finish the work that was waiting on each one. Then, for each request, handle it the way you would handle a request from your owner, and call reply with that request's id and your result. Call check_inbox again and keep going until it returns nothing waiting, so this run drains the whole inbox. If you need something from a teammate yourself, call ask; it may return before the answer does, and you do not have to wait for it: you will be woken again when the reply arrives. You can read anywhere, but write only in your working directory and in write roots your operator has opened; if work needs to be written somewhere else, say so in your reply rather than writing there."

# Grok's output is kept in the wake's private state directory, so an
# authentication failure can be told apart from other failures, then
# passed on to the listener's log.
state=$(CDPATH='' cd -P -- "$TINCAN_WAKE_STATE_DIR" && pwd -P)
out=$state/out.$$
err=$state/err.$$
status=0
tincan_wake_run env HOME="$WAKE_HOME" GROK_HOME="$GROK_HOME_DIR" GROK_DISABLE_AUTOUPDATER=1 \
  "$BIN" -p "$PROMPT" --output-format json --always-approve --sandbox "$PROFILE" \
  --cwd "$workdir_c" --session-id "$SID" >"$out" 2>"$err" || status=$?
cat "$out"
cat "$err" >&2
# Record the id Grok reports too, should it differ from the one asked for.
reported=$(sed -n 's/.*"sessionId": *"\([0-9a-f-]*\)".*/\1/p' "$out" | head -n 1)
if [ -n "$reported" ] && [ "$reported" != "$SID" ] && [ -w "$SESSIONS" ]; then
  printf '%s\n' "$reported" >>"$SESSIONS"
fi
auth=
if [ "$status" -ne 0 ] && [ "$status" -ne 124 ]; then
  # Only Grok's own error object and its stderr: a successful answer
  # carries the model's words, which can say anything.
  if { grep -E '^[[:space:]]*\{"type": *"error"' "$out" || true; cat "$err"; } |
    grep -Eiq 'not logged in|not authenticated|unauthenticated|authentication (failed|required|expired)|unauthorized|401|log ?in (again|required)|sign in again|please (re-?)?(log|sign) ?in|invalid api key|XAI_API_KEY'; then
    auth=1
  fi
fi
rm -f "$out" "$err"
if [ -n "$auth" ]; then
  tincan_wake_backoff "Grok Build is not logged in or its login expired; run GROK_HOME=$GROK_HOME_DIR grok login on this machine, or check XAI_API_KEY in the listener's environment"
fi
exit "$status"
