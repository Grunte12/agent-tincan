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
#   GROK_BIN                     the grok binary (default grok on PATH). The
#                                wake resolves it to one file and runs only
#                                that file; for the npm package's node
#                                bootstrap, that is the native binary the
#                                owner's Grok home points at
#                                ($GROK_HOME/bin/grok, ~/.grok/bin/grok).
#                                A binary under the wake home, the
#                                workdir, a write root or a temp directory
#                                is refused.
#   TINCAN_WAKE_STATE_DIR        as in the library, but by default
#                                <name>.wake-state beside TINCAN_CONFIG,
#                                outside the temp directories the run can
#                                write
set -eu

: "${TINCAN_CONFIG:=$HOME/.config/tincan/grok-cli.json}"
# shellcheck disable=SC2088 # a literal ~/ prefix, which tincan expands
case $TINCAN_CONFIG in "~/"*) TINCAN_CONFIG=$HOME/${TINCAN_CONFIG#"~/"} ;; esac
# The run is started from the workdir, so every path the wake keeps is
# made absolute first.
case $TINCAN_CONFIG in /*) ;; *) TINCAN_CONFIG=$(pwd -P)/$TINCAN_CONFIG ;; esac
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
# The owner's Grok home, where the npm bootstrap finds the native binary.
OWNER_GROK_HOME=${GROK_HOME:-$OWNER_HOME/.grok}
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

# The run can write the temp directories, so the lock, failure count and
# saved listing live beside the teammate's config by default (mode 700).
: "${TINCAN_WAKE_STATE_DIR:=$cfg_dir/$cfg_name.wake-state}"
case $TINCAN_WAKE_STATE_DIR in /*) ;; *) TINCAN_WAKE_STATE_DIR=$(pwd -P)/$TINCAN_WAKE_STATE_DIR ;; esac

tincan_wake_init grok-cli
tincan_wake_begin

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

# grok_resolve PATH prints the canonical path of the file PATH names, with
# every symlink followed, or fails.
grok_resolve() {
  _p=$1
  _n=0
  while [ -L "$_p" ]; do
    _n=$((_n + 1))
    [ "$_n" -le 40 ] || return 1
    _t=$(readlink "$_p") || return 1
    case $_t in
      /*) _p=$_t ;;
      *) _p=$(dirname -- "$_p")/$_t ;;
    esac
  done
  [ -f "$_p" ] || return 1
  _d=$(tincan_wake_canonical_dir "$(dirname -- "$_p")") || return 1
  printf '%s/%s\n' "${_d%/}" "$(basename -- "$_p")"
}

# grok_is_node_script FILE succeeds if FILE starts with a node shebang, as
# the npm package's grok bootstrap does.
grok_is_node_script() {
  head -c 256 "$1" 2>/dev/null | head -n 1 | grep -Eq '^#!.*[/ ]node([[:space:]]|$)'
}

# Pin the binary. The npm install of Grok Build puts a node bootstrap on
# PATH that runs $GROK_HOME/bin/grok, which under the wake's GROK_HOME is a
# file the sandboxed run can write. So the wake resolves the file once,
# follows the bootstrap to the native binary the owner's Grok home points
# at, and runs that one file for the version check, the listing and the
# run: the vetted file is the executed file.
bin_path=$(command -v "$BIN" 2>/dev/null) || tincan_wake_refuse "$BIN is not installed or not on PATH"
case $bin_path in
  /*) ;;
  */*) bin_path=$(pwd -P)/$bin_path ;;
  *) tincan_wake_refuse "$BIN is not a program on PATH (an alias, function or builtin); set GROK_BIN to the grok binary's path" ;;
esac
GROK_EXE=$(grok_resolve "$bin_path") || tincan_wake_refuse "cannot resolve the grok binary $bin_path"
if grok_is_node_script "$GROK_EXE"; then
  native=$OWNER_GROK_HOME/bin/grok
  if ! GROK_EXE=$(grok_resolve "$native") || grok_is_node_script "$GROK_EXE"; then
    tincan_wake_refuse "$bin_path is Grok Build's npm bootstrap and $native is not a native grok binary; set GROK_BIN to the native binary (for example ~/.grok/bin/grok-X.Y.Z)"
  fi
  tincan_wake_log "$bin_path is Grok Build's npm bootstrap; running the native binary it uses for $OWNER_GROK_HOME, $GROK_EXE"
fi
if [ ! -x "$GROK_EXE" ]; then
  tincan_wake_refuse "the grok binary $GROK_EXE is not executable"
fi
# The run can also write the temp directories: TMPDIR, /tmp, /var/tmp and,
# on macOS, the per-user temp root (DARWIN_USER_TEMP_DIR and the
# /var/folders directory above it). Roots that do not exist are skipped.
temp_roots=${TMPDIR:-}:/tmp:/private/tmp:/var/tmp:/private/var/tmp
if darwin_tmp=$(getconf DARWIN_USER_TEMP_DIR 2>/dev/null) && [ -n "$darwin_tmp" ]; then
  darwin_tmp=${darwin_tmp%/}
  temp_roots=$temp_roots:$darwin_tmp:$(dirname -- "$darwin_tmp")
fi
if tincan_wake_under_root "$GROK_EXE" "$wake_home_c:$workdir_c:$(printf '%s' "$roots" | tr '\n' ':'):$temp_roots"; then
  tincan_wake_refuse "the grok binary $GROK_EXE is where the sandboxed run can write (the wake home, the workdir, a write root or a temp directory); install Grok Build elsewhere or set GROK_BIN to a binary outside them"
fi
# Nothing runs a grok from the wake home; one found there is removed.
if [ -e "$GROK_HOME_DIR/bin" ] || [ -L "$GROK_HOME_DIR/bin" ]; then
  tincan_wake_log "removing $GROK_HOME_DIR/bin; the wake runs $GROK_EXE"
  rm -rf "${GROK_HOME_DIR:?}/bin" || tincan_wake_refuse "cannot remove $GROK_HOME_DIR/bin"
fi

tincan_wake_check_binary env "$VERSION_PATTERN" HOME="$WAKE_HOME" GROK_HOME="$GROK_HOME_DIR" \
  GROK_DISABLE_AUTOUPDATER=1 "$GROK_EXE" --version

# A login lives in the wake home's auth.json; XAI_API_KEY is the other way.
if [ -z "${XAI_API_KEY:-}" ] && [ ! -s "$GROK_HOME_DIR/auth.json" ]; then
  tincan_wake_refuse "Grok Build is not logged in for this wake: run GROK_HOME=$GROK_HOME_DIR grok login once on this machine, or set XAI_API_KEY in the listener's environment"
fi

# grok_env runs a command with the wake home as HOME and GROK_HOME, and
# Grok's update check off (an update mid-run would change the binary the
# version check vetted). The version check and the run set the same.
# shellcheck disable=SC2329 # run through grok_mcp_list
grok_env() {
  HOME=$WAKE_HOME GROK_HOME=$GROK_HOME_DIR GROK_DISABLE_AUTOUPDATER=1 "$@"
}

INSPECT=$TINCAN_WAKE_STATE_DIR/inspect.json

# Grok also loads MCP servers from a project's .grok/config.toml and
# .mcp.json, above its cwd up to the project root, and grok inspect shows a
# project server that overrides the wake home's entry with the wake home's
# file as its source. The model can write the workdir, so any such file in
# the workdir or a directory above it stops the wake. The wake home's own
# config.toml and the owner's Grok config (a user config, not a project
# one, when HOME is the wake home) are the exceptions; a project layer
# Grok still reports is refused after the listing.
grok_home_c=$(tincan_wake_canonical_dir "$GROK_HOME_DIR") || grok_home_c=$wake_home_c/.grok
owner_grok_c=$(tincan_wake_canonical_dir "$OWNER_GROK_HOME") || owner_grok_c=
d=$workdir_c
while :; do
  for f in "${d%/}/.grok/config.toml" "${d%/}/.mcp.json"; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    case $f in
      "$grok_home_c/config.toml") continue ;;
      "$owner_grok_c/config.toml") [ -n "$owner_grok_c" ] && continue ;;
    esac
    tincan_wake_refuse "found project MCP config $f in or above the workdir $workdir_c; Grok would load its servers with approval off, so remove it or pick another TINCAN_GROK_WORKDIR"
  done
  [ "$d" != / ] || break
  d=$(dirname -- "$d")
done

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

# grok_project_layers prints the project config files the saved inspect
# output says Grok loads, one per line.
grok_project_layers() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -r '(.configSources.layers // [])[] | select(.role == "project") | .path // "?" | tostring' "$INSPECT"
  else
    python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
for l in (d.get("configSources") or {}).get("layers") or []:
    if l.get("role") == "project":
        print(l.get("path") or "?")
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

# grok_toml_entry FILE NAME TARGET prints "TINCAN_CONFIG<TAB>command args"
# for the [mcp_servers.NAME] table of a Grok config.toml, reading its env
# either as an [mcp_servers.NAME.env] table or inline (env = {
# TINCAN_CONFIG = ... }). TARGET is the command grok inspect says it runs;
# when the entry's command is not TARGET, the entry is not what Grok
# loaded (a project config overrides it), so it prints an empty
# TINCAN_CONFIG and TARGET. Only one-line values are read; anything it
# cannot read comes out empty, which the identity check refuses.
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
END {
  if (cmd != target) { cfg = ""; cmd = target; args = "" }
  printf "%s\t%s%s\n", cfg, cmd, args
}
'
# shellcheck disable=SC2329 # run through grok_mcp_list
grok_toml_entry() {
  awk -v want="$2" -v target="$3" "$_grok_toml_awk" "$1"
}

# grok_mcp_list prints the MCP servers Grok discovers for the workdir, run
# the way the wake runs it, as the library's listing lines: name,
# TINCAN_CONFIG, command. Grok's listing does not show a server's env, so
# for a server from a config.toml it is read from that file's entry, when
# that entry's command is the one Grok reports; any other source (a Claude
# or Cursor import, a plugin) has none.
# shellcheck disable=SC2329 # run by tincan_wake_check_identity
grok_mcp_list() {
  (cd "$workdir_c" && grok_env "$GROK_EXE" inspect --json) >"$INSPECT" || return 1
  _us=$(printf '\037')
  grok_inspect_rows | while IFS=$_us read -r _name _type _path _target; do
    [ -n "$_name$_type$_path$_target" ] || continue
    _entry=$(printf '\t%s' "$_target")
    case $_path in
      *.toml) [ -f "$_path" ] && _entry=$(grok_toml_entry "$_path" "$_name" "$_target") ;;
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
layers=$(grok_project_layers 2>/dev/null) || layers="(unreadable)"
if [ -n "$layers" ]; then
  tincan_wake_refuse "grok loads project config from $(printf '%s' "$layers" | tr '\n' ' ')for the workdir $workdir_c; its MCP servers would run with approval off, so remove it or pick another TINCAN_GROK_WORKDIR"
fi
rm -f "$INSPECT"

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
# The run starts in the workdir, as the listing did, so both see the same
# project config.
cd "$workdir_c" || tincan_wake_refuse "cannot enter the workdir $workdir_c"
tincan_wake_run env HOME="$WAKE_HOME" GROK_HOME="$GROK_HOME_DIR" GROK_DISABLE_AUTOUPDATER=1 \
  "$GROK_EXE" -p "$PROMPT" --output-format json --always-approve --sandbox "$PROFILE" \
  --cwd "$workdir_c" --session-id "$SID" >"$out" 2>"$err" || status=$?
cat "$out"
cat "$err" >&2
# Record the id Grok reports too, should it differ from the one asked for.
reported=$(sed -n 's/.*"sessionId": *"\([0-9a-f-]*\)".*/\1/p' "$out" | head -n 1)
if [ -n "$reported" ] && [ "$reported" != "$SID" ] && [ -w "$SESSIONS" ]; then
  printf '%s\n' "$reported" >>"$SESSIONS"
fi
# grok_error_message prints the message of each error object in Grok's
# JSON output: only the message, since the object can also carry token
# counts and request ids.
grok_error_message() {
  if [ "$JSON_TOOL" = jq ]; then
    jq -rR 'fromjson? | select(type == "object" and .type == "error") | .message // "" | tostring' "$1"
  else
    python3 -c '
import json, sys
for line in open(sys.argv[1], errors="replace"):
    try:
        d = json.loads(line)
    except ValueError:
        continue
    if isinstance(d, dict) and d.get("type") == "error":
        print(str(d.get("message") or ""))
' "$1"
  fi
}
auth=
if [ "$status" -ne 0 ] && [ "$status" -ne 124 ]; then
  # Only Grok's own error message and its stderr: a successful answer
  # carries the model's words, which can say anything. 401 counts only as
  # a number of its own, never inside a count or after the decimal point
  # of a timestamp (.401Z, ,401).
  if { grok_error_message "$out" 2>/dev/null || true; cat "$err"; } |
    grep -Eiq 'not logged in|not authenticated|unauthenticated|authentication (failed|required|expired)|unauthorized|(^|[^0-9.,])401([^0-9]|$)|log ?in (again|required)|sign in again|please (re-?)?(log|sign) ?in|invalid api key|XAI_API_KEY'; then
    auth=1
  fi
fi
rm -f "$out" "$err"
if [ -n "$auth" ]; then
  tincan_wake_backoff "Grok Build is not logged in or its login expired; run GROK_HOME=$GROK_HOME_DIR grok login on this machine, or check XAI_API_KEY in the listener's environment"
fi
exit "$status"
