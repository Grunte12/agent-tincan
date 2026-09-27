# shellcheck shell=sh
# Shared wake library for command-woken CLI teammates.
#
# A per-CLI wake script (run by "tincan listen --exec") sources this file and
# calls its functions in order. It is plain POSIX sh, so it works from
# /bin/sh on macOS and Linux. It is not in the release downloads: copy it
# from the repo (examples/lib/tincan-wake-lib.sh) next to the wake script
# that sources it. A per-CLI script looks like:
#
#   . "$(dirname "$0")/tincan-wake-lib.sh"
#   tincan_wake_init mycli                  # names, defaults, state dir
#   tincan_wake_begin                       # lock and backoff; may exit 0
#   tincan_wake_check_binary "$BIN" '^MyCLI '
#   mycli_mcp_list() { ... }                # prints the CLI's MCP listing
#   tincan_wake_check_identity mycli_mcp_list
#   roots=$(tincan_wake_write_roots "$ALLOWED_ROOTS" "$WRITE_ROOTS")
#   set -- "$BIN" <headless and sandbox flags>   # plus one flag per root
#   tincan_wake_run "$@" "$PROMPT"          # timeout, failure count
#
# Operator settings, read from the environment (the listener's):
#
#   TINCAN_CONFIG                  this teammate's tincan config (required)
#   TINCAN_WAKE_STATE_DIR          lock, failure count and backoff marker
#                                  (default ${TMPDIR:-/tmp}/tincan-<name>-wake)
#   TINCAN_WAKE_TIMEOUT            seconds one CLI run may take (default 1500)
#   TINCAN_WAKE_MAX_FAILURES       failures in a row before backing off (3)
#   TINCAN_WAKE_BACKOFF            seconds to back off (default 3600)
#   TINCAN_WAKE_OPERATOR           teammate told once when the wake backs
#                                  off (default none: no notice is sent)
#   TINCAN_WAKE_ALLOWED_SERVERS    MCP servers other than tincan the CLI may
#                                  have, comma or space separated (default
#                                  none). The CLI runs with tool approval
#                                  off, so every listed server is trusted.
#   TINCAN_BIN                     the tincan binary (default tincan)
#
# Every function keeps its own variables under the _tw_ prefix, since POSIX
# sh has no local variables.

# tincan_wake_init NAME sets the defaults. NAME names the wake in log lines
# and its default state directory.
tincan_wake_init() {
  _tw_name=$1
  : "${TINCAN_WAKE_STATE_DIR:=${TMPDIR:-/tmp}/tincan-$_tw_name-wake}"
  : "${TINCAN_WAKE_TIMEOUT:=1500}"
  : "${TINCAN_WAKE_MAX_FAILURES:=3}"
  : "${TINCAN_WAKE_BACKOFF:=3600}"
  : "${TINCAN_WAKE_OPERATOR:=}"
  : "${TINCAN_WAKE_ALLOWED_SERVERS:=}"
  : "${TINCAN_BIN:=tincan}"
  _tw_state=${TINCAN_WAKE_STATE_DIR%/}
  _tw_lock=$_tw_state/lock
  _tw_locked=
  _tw_child=
  for _tw_v in "$TINCAN_WAKE_TIMEOUT" "$TINCAN_WAKE_MAX_FAILURES" "$TINCAN_WAKE_BACKOFF"; do
    case $_tw_v in
      '' | *[!0-9]*)
        tincan_wake_log "TINCAN_WAKE_TIMEOUT, TINCAN_WAKE_MAX_FAILURES and TINCAN_WAKE_BACKOFF must be whole numbers"
        exit 2
        ;;
    esac
  done
}

tincan_wake_log() {
  echo "$_tw_name-wake: $*" >&2
}

# tincan_wake_begin takes the lock and checks the backoff marker. When a run
# is already in progress, or the wake is backing off, it exits 0 and the
# requests stay queued for a later nudge.
tincan_wake_begin() {
  if ! tincan_wake_lock; then
    tincan_wake_log "a run is already in progress, leaving requests queued"
    exit 0
  fi
  if tincan_wake_backoff_active; then
    tincan_wake_log "backing off after failures until $(_tw_date "$_tw_until"), leaving requests queued"
    exit 0
  fi
}

# tincan_wake_lock takes the lock directory (mkdir is atomic) and records
# this script's pid in it. A lock whose pid is gone was left by a run that
# was killed, and is broken. So is one older than a run can take, whatever
# its pid says, since pids are reused. Returns 1 when a live run holds it.
tincan_wake_lock() {
  mkdir -p "$_tw_state" && chmod 700 "$_tw_state" || return 1
  trap '_tw_cleanup' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if ! mkdir "$_tw_lock" 2>/dev/null; then
    _tw_pid=$(cat "$_tw_lock/pid" 2>/dev/null) || _tw_pid=
    _tw_max=$((TINCAN_WAKE_TIMEOUT / 60 + 5))
    if [ -n "$_tw_pid" ] && kill -0 "$_tw_pid" 2>/dev/null && ! _tw_older "$_tw_lock" "$_tw_max"; then
      return 1
    fi
    # No pid yet: another wake made the lock a moment ago.
    if [ -z "$_tw_pid" ] && ! _tw_older "$_tw_lock" 1; then
      return 1
    fi
    # Move the stale lock aside under a name only this run uses, and put it
    # back if another wake replaced it in the meantime.
    _tw_stale=$_tw_lock.stale.$$
    mv "$_tw_lock" "$_tw_stale" 2>/dev/null || return 1
    if [ "$(cat "$_tw_stale/pid" 2>/dev/null)" != "$_tw_pid" ]; then
      mv "$_tw_stale" "$_tw_lock" 2>/dev/null || rm -rf "$_tw_stale"
      return 1
    fi
    rm -rf "$_tw_stale"
    tincan_wake_log "broke a stale lock left by pid ${_tw_pid:-unknown}"
    mkdir "$_tw_lock" 2>/dev/null || return 1
  fi
  echo $$ >"$_tw_lock/pid"
  _tw_locked=1
}

# _tw_older DIR MINUTES succeeds if DIR was last changed more than MINUTES
# minutes ago.
_tw_older() {
  [ -n "$(find "$1" -maxdepth 0 -mmin +"$2" 2>/dev/null)" ]
}

_tw_cleanup() {
  if [ -n "$_tw_child" ]; then
    _tw_kill_tree KILL "$_tw_child"
  fi
  if [ -n "$_tw_locked" ] && [ "$(cat "$_tw_lock/pid" 2>/dev/null)" = "$$" ]; then
    rm -rf "$_tw_lock"
  fi
}

_tw_now() {
  date +%s
}

_tw_date() {
  date -r "$1" 2>/dev/null || date -d "@$1" 2>/dev/null || echo "epoch $1"
}

# tincan_wake_backoff_active succeeds while the backoff marker is in force,
# leaving its end time in _tw_until. A marker that has run out is removed;
# the failure count stays, so one more failure backs off again.
tincan_wake_backoff_active() {
  _tw_until=$(cat "$_tw_state/backoff" 2>/dev/null) || return 1
  case $_tw_until in '' | *[!0-9]*) _tw_until=0 ;; esac
  if [ "$(_tw_now)" -lt "$_tw_until" ]; then
    return 0
  fi
  rm -f "$_tw_state/backoff"
  return 1
}

# _tw_backoff REASON sets the backoff marker and tells the operator.
_tw_backoff() {
  _tw_until=$(($(_tw_now) + TINCAN_WAKE_BACKOFF))
  echo "$_tw_until" >"$_tw_state/backoff"
  tincan_wake_log "backing off until $(_tw_date "$_tw_until"): $1"
  tincan_wake_notify "The $_tw_name wake stopped running and is backing off until $(_tw_date "$_tw_until"): $1. Requests to it stay queued. Check the wake's log on its machine; one successful run clears this."
}

# tincan_wake_notify MESSAGE sends MESSAGE to TINCAN_WAKE_OPERATOR, as this
# teammate, once until a run succeeds. Nothing is sent when no operator is
# set, and a failed send is only logged.
tincan_wake_notify() {
  [ -n "$TINCAN_WAKE_OPERATOR" ] || return 0
  [ ! -e "$_tw_state/notified" ] || return 0
  if "$TINCAN_BIN" ask --notify "$TINCAN_WAKE_OPERATOR" "$1" >/dev/null 2>&1 </dev/null; then
    : >"$_tw_state/notified"
  else
    tincan_wake_log "could not notify $TINCAN_WAKE_OPERATOR"
  fi
}

# tincan_wake_refuse REASON stops this wake before the CLI runs: it backs
# off, tells the operator, and exits 1. For problems a retry cannot fix.
tincan_wake_refuse() {
  tincan_wake_log "refusing to run: $1"
  _tw_backoff "$1"
  exit 1
}

# tincan_wake_record_failure REASON counts one failed run, and backs off
# once TINCAN_WAKE_MAX_FAILURES runs in a row have failed.
tincan_wake_record_failure() {
  _tw_n=$(cat "$_tw_state/failures" 2>/dev/null) || _tw_n=0
  case $_tw_n in '' | *[!0-9]*) _tw_n=0 ;; esac
  _tw_n=$((_tw_n + 1))
  echo "$_tw_n" >"$_tw_state/failures"
  tincan_wake_log "run failed ($1), $_tw_n in a row"
  if [ "$_tw_n" -ge "$TINCAN_WAKE_MAX_FAILURES" ]; then
    _tw_backoff "$_tw_n runs in a row failed, the last one: $1"
  fi
}

# tincan_wake_record_success clears the failure count, the backoff marker
# and the notice marker.
tincan_wake_record_success() {
  rm -f "$_tw_state/failures" "$_tw_state/backoff" "$_tw_state/notified"
}

# tincan_wake_check_binary BIN PATTERN [ARGS...] runs BIN ARGS (default
# --version) and refuses to wake unless its output matches the extended
# regular expression PATTERN, so a different tool of the same name never
# runs with this teammate's tools.
tincan_wake_check_binary() {
  _tw_bin=$1
  _tw_pattern=$2
  shift 2
  [ $# -gt 0 ] || set -- --version
  if ! command -v "$_tw_bin" >/dev/null 2>&1; then
    tincan_wake_refuse "$_tw_bin is not installed or not on PATH"
  fi
  if ! _tw_out=$("$_tw_bin" "$@" 2>&1 </dev/null); then
    tincan_wake_refuse "$_tw_bin $* failed: $(printf '%s\n' "$_tw_out" | head -n 1)"
  fi
  if ! printf '%s\n' "$_tw_out" | grep -Eq -- "$_tw_pattern"; then
    tincan_wake_refuse "$_tw_bin is not the expected tool: $_tw_bin $* says \"$(printf '%s\n' "$_tw_out" | head -n 1)\""
  fi
}

# tincan_wake_check_identity LISTFN pins this wake to its own teammate. The
# per-CLI function LISTFN prints the CLI's MCP servers, one per line, as
#
#   name<TAB>TINCAN_CONFIG from the server's env (empty if none)<TAB>command and args
#
# A server is agent-tincan when its name or its command mentions tincan.
# The wake refuses to run unless there is exactly one, its TINCAN_CONFIG is
# this wake's TINCAN_CONFIG, and every other server is in
# TINCAN_WAKE_ALLOWED_SERVERS: the CLI runs with tool approval off, so it
# must not be able to act as another teammate or through an unvetted server.
tincan_wake_check_identity() {
  if [ -z "${TINCAN_CONFIG:-}" ]; then
    tincan_wake_refuse "TINCAN_CONFIG is not set for this wake"
  fi
  if ! _tw_listing=$("$1"); then
    tincan_wake_refuse "could not list the CLI's MCP servers"
  fi
  _tw_allowed=$(printf '%s' "$TINCAN_WAKE_ALLOWED_SERVERS" | tr ',' ' ')
  # awk -F '\t' keeps an empty TINCAN_CONFIG field in place, where the
  # shell's read would collapse the two tabs around it.
  # shellcheck disable=SC2016 # awk program text, expanded by awk
  _tw_tincan_awk='$1 != "" && (tolower($1) ~ /tincan/ || $3 ~ /tincan/)'
  _tw_count=$(printf '%s\n' "$_tw_listing" | awk -F '\t' "$_tw_tincan_awk { n++ } END { print n + 0 }")
  _tw_tincan=$(printf '%s\n' "$_tw_listing" | awk -F '\t' "$_tw_tincan_awk { print \$2; exit }")
  _tw_extra=$(printf '%s\n' "$_tw_listing" | awk -F '\t' -v allowed="$_tw_allowed" "
    BEGIN { n = split(allowed, a, \" \"); for (i = 1; i <= n; i++) ok[a[i]] = 1 }
    \$1 == \"\" || ($_tw_tincan_awk) { next }
    !(\$1 in ok) { printf \"%s%s\", sep, \$1; sep = \", \" }")
  if [ "$_tw_count" -ne 1 ]; then
    tincan_wake_refuse "the CLI has $_tw_count agent-tincan servers; it needs exactly one, with TINCAN_CONFIG=$TINCAN_CONFIG"
  fi
  if [ -z "$_tw_tincan" ]; then
    tincan_wake_refuse "the CLI's agent-tincan server sets no TINCAN_CONFIG; set it to $TINCAN_CONFIG"
  fi
  if ! _tw_same_file "$_tw_tincan" "$TINCAN_CONFIG"; then
    tincan_wake_refuse "the CLI's agent-tincan server uses TINCAN_CONFIG=$_tw_tincan, not this wake's $TINCAN_CONFIG"
  fi
  if [ -n "$_tw_extra" ]; then
    tincan_wake_refuse "the CLI has MCP servers the operator did not allow: $_tw_extra (list them in TINCAN_WAKE_ALLOWED_SERVERS, or remove them)"
  fi
}

# _tw_same_file A B succeeds if A and B are the same path, or name the same
# file once their directories are resolved.
_tw_same_file() {
  [ "$1" = "$2" ] && return 0
  _tw_a=$(tincan_wake_canonical_dir "$(dirname -- "$1")") || return 1
  _tw_b=$(tincan_wake_canonical_dir "$(dirname -- "$2")") || return 1
  [ "$_tw_a/$(basename -- "$1")" = "$_tw_b/$(basename -- "$2")" ]
}

# tincan_wake_canonical_dir prints the physical path of an existing
# directory given by absolute path, or fails. macOS has no realpath -m, so
# it resolves with cd -P in a subshell.
tincan_wake_canonical_dir() {
  case "$1" in
    /*) ;;
    *) return 1 ;;
  esac
  (CDPATH='' cd -P -- "$1" 2>/dev/null && pwd -P)
}

# tincan_wake_under_root CANON ROOTS succeeds if canonical path CANON is at
# or under one of ROOTS (colon-separated, each canonicalized).
tincan_wake_under_root() {
  _tw_canon=$1
  _tw_old_ifs=$IFS
  IFS=:
  set -f
  # shellcheck disable=SC2086 # split on IFS=: on purpose
  set -- $2
  set +f
  IFS=$_tw_old_ifs
  for _tw_root in "$@"; do
    [ -n "$_tw_root" ] || continue
    _tw_root_canon=$(tincan_wake_canonical_dir "$_tw_root") || continue
    [ "$_tw_root_canon" = / ] && _tw_root_canon=
    case "$_tw_canon/" in
      "$_tw_root_canon"/*) return 0 ;;
    esac
  done
  return 1
}

# tincan_wake_write_roots ALLOWED WRITE prints, one per line, the canonical
# path of each entry of WRITE (colon-separated absolute paths the operator
# opened) that is at or under a root in ALLOWED (colon-separated). Anything
# else, a relative or missing path, a path outside every allowed root, a ../
# or symlink that climbs out of one, is skipped with a note on stderr. The
# per-CLI script turns each line into its CLI's write-root flag; nothing
# the model says can add one.
tincan_wake_write_roots() {
  _tw_allowed_roots=$1
  _tw_old_ifs=$IFS
  IFS=:
  set -f
  # shellcheck disable=SC2086 # split on IFS=: on purpose
  set -- $2
  set +f
  IFS=$_tw_old_ifs
  for _tw_w in "$@"; do
    [ -n "$_tw_w" ] || continue
    if ! _tw_wc=$(tincan_wake_canonical_dir "$_tw_w"); then
      tincan_wake_log "skipping write root $_tw_w: not an absolute path to an existing directory"
      continue
    fi
    case $_tw_wc in
      *'
'*)
        tincan_wake_log "skipping write root $_tw_w: its path has a newline"
        continue
        ;;
    esac
    if tincan_wake_under_root "$_tw_wc" "$_tw_allowed_roots"; then
      printf '%s\n' "$_tw_wc"
    else
      tincan_wake_log "refusing write root $_tw_w (resolves to $_tw_wc): not under an allowed root ($_tw_allowed_roots)"
    fi
  done
}

# _tw_tree PID prints PID and all its descendants.
_tw_tree() {
  _tw_queue=$1
  _tw_all=
  while :; do
    # shellcheck disable=SC2086
    set -- $_tw_queue
    [ $# -gt 0 ] || break
    _tw_p=$1
    shift
    _tw_queue=$*
    _tw_all="$_tw_all $_tw_p"
    if command -v pgrep >/dev/null 2>&1; then
      _tw_queue="$_tw_queue $(pgrep -P "$_tw_p" 2>/dev/null | tr '\n' ' ')"
    fi
  done
  # shellcheck disable=SC2086
  echo $_tw_all
}

# _tw_kill_tree SIGNAL PID signals PID and its descendants, collected first
# so none is reparented out of reach.
_tw_kill_tree() {
  _tw_pids=$(_tw_tree "$2")
  # shellcheck disable=SC2086
  kill -"$1" $_tw_pids 2>/dev/null || true
}

# tincan_wake_run CMD [ARGS...] runs the CLI with stdin closed and a hard
# TINCAN_WAKE_TIMEOUT (GNU timeout is not on macOS, so a watchdog does it:
# TERM to the CLI and everything it started, KILL five seconds later). A
# clean exit clears the failure count; anything else counts as a failure.
# Returns the CLI's status, or 124 when it timed out.
tincan_wake_run() {
  _tw_timed_out=$_tw_state/timed-out.$$
  rm -f "$_tw_timed_out"
  "$@" </dev/null &
  _tw_child=$!
  (
    _tw_i=0
    while [ "$_tw_i" -lt "$TINCAN_WAKE_TIMEOUT" ]; do
      sleep 1
      kill -0 "$_tw_child" 2>/dev/null || exit 0
      _tw_i=$((_tw_i + 1))
    done
    : >"$_tw_timed_out"
    _tw_kill_tree TERM "$_tw_child"
    sleep 5
    _tw_kill_tree KILL "$_tw_child"
  ) </dev/null >/dev/null 2>&1 &
  _tw_watchdog=$!
  _tw_status=0
  wait "$_tw_child" || _tw_status=$?
  _tw_child=
  kill "$_tw_watchdog" 2>/dev/null || true
  wait "$_tw_watchdog" 2>/dev/null || true
  if [ -e "$_tw_timed_out" ]; then
    rm -f "$_tw_timed_out"
    tincan_wake_record_failure "timed out after $TINCAN_WAKE_TIMEOUT seconds"
    return 124
  fi
  if [ "$_tw_status" -eq 0 ]; then
    tincan_wake_record_success
    return 0
  fi
  tincan_wake_record_failure "exit status $_tw_status"
  return "$_tw_status"
}
