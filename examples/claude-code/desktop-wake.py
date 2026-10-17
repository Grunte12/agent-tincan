#!/usr/bin/env python3
"""Wake an existing Desktop session via native hooks; never claim or send work."""
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import shlex
import signal
import stat
import subprocess
import sys
import time
import uuid
import re

REGISTRY = Path.home() / '.config/tincan/claude-session-routes.json'
STATE = Path.home() / '.config/tincan/desktop-wake'
TINCAN = Path.home() / '.local/bin/tincan'
RECEIVE_LEASE_SECONDS = 23 * 60 * 60
NOTIFICATION_ID = re.compile(r'^[0-9a-f-]{8,64}$', re.I)
_discovery_path = Path(__file__).with_name('desktop-sessions.py')
if not _discovery_path.exists():
    _discovery_path = Path.home() / '.local/share/tincan/adapters/claude-code/desktop-sessions.py'
_spec = importlib.util.spec_from_file_location('desktop_sessions', _discovery_path)
discovery = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(discovery)

def private_file(path, flags):
    fd = os.open(path, flags | os.O_NOFOLLOW, 0o600)
    info = os.fstat(fd)
    if info.st_uid != os.getuid() or not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
        os.close(fd)
        raise ValueError('unsafe_private_file')
    return fd

def route_for(sid, cwd=None):
    project = discovery.project_for(sid, REGISTRY, STATE)
    if cwd is not None and not Path(cwd).resolve(strict=True).is_relative_to(project):
        raise ValueError('wrong_project')
    return project

def context(mode, item=None):
    if item is None:
        item = json.load(sys.stdin)
    if item.get('agent_id') or item.get('agent_type'):
        raise ValueError('subagent_not_owner')
    sid = item['session_id']
    transcript = Path(item['transcript_path'])
    if discovery.route._session_uuid(sid) != sid or not transcript.is_absolute() or transcript.name != sid + '.jsonl':
        raise ValueError('wrong_transcript')
    if mode in ('prepare', 'recover'):
        events = ('SessionStart', 'CwdChanged') if mode == 'prepare' else ('SessionStart', 'Stop')
        if item.get('hook_event_name') not in events:
            raise ValueError('not_a_discovery_event')
        project = discovery.register(item, REGISTRY, STATE)
    else:
        project = route_for(sid)
    return item, sid, project

def recovery_allowed(item, policy_path=None):
    """Operator-gated supplementary observer; excluded chats have no writes."""
    path = policy_path if policy_path is not None else STATE / 'recovery-policy.json'
    if not path.exists():
        return False
    with os.fdopen(private_file(path, os.O_RDONLY)) as stream:
        policy = json.load(stream)
    if (not isinstance(policy, dict)
            or set(policy) != {'enabled', 'excluded_session_ids', 'excluded_projects', 'allowed_session_ids'}
            or type(policy['enabled']) is not bool):
        raise ValueError('invalid_recovery_policy')
    for key in ('excluded_session_ids', 'allowed_session_ids'):
        ids = policy[key]
        if not isinstance(ids, list) or any(not isinstance(s, str) or discovery.route._session_uuid(s) != s for s in ids):
            raise ValueError('invalid_recovery_policy')
    projects = policy['excluded_projects']
    if not isinstance(projects, list) or any(not isinstance(p, str) or not Path(p).is_absolute() or os.path.normpath(p) != p for p in projects):
        raise ValueError('invalid_recovery_policy')
    sid = item.get('session_id')
    if not policy['enabled'] or sid in policy['excluded_session_ids']:
        return False
    if policy['allowed_session_ids'] and sid not in policy['allowed_session_ids']:
        return False
    cwd = Path(item['cwd']).resolve(strict=True)
    if any(cwd.is_relative_to(Path(p)) for p in projects):
        return False
    return True

def state_directory():
    STATE.mkdir(mode=0o700, exist_ok=True)
    info = STATE.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError('unsafe_state_directory')

def write_json(path, item):
    with os.fdopen(private_file(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC), 'w') as stream:
        json.dump(item, stream)
        stream.write('\n')

def _notification_key(item):
    if (not isinstance(item, dict) or not NOTIFICATION_ID.fullmatch(item.get('id', ''))
            or discovery.route._session_uuid(item.get('session_id', '')) != item.get('session_id')
            or item.get('kind') not in ('request', 'reply')):
        raise ValueError('invalid_notification')
    generation = item.get('generation', 0)
    if item['kind'] == 'reply' and (type(generation) is not int or generation < 0):
        raise ValueError('invalid_notification')
    return ':'.join((item['session_id'], item['kind'], item['id'], str(generation)))

def _notification_state():
    path = STATE / 'notification-keys.json'
    if not path.exists():
        return []
    with os.fdopen(private_file(path, os.O_RDONLY)) as stream:
        value = json.load(stream)
    if not isinstance(value, list) or any(not isinstance(key, str) for key in value):
        raise ValueError('invalid_notification_state')
    return value

def reset_notification_state(sid):
    """Let an existing queued item wake a session again after native reopen."""
    with os.fdopen(private_file(STATE / 'notification.lock', os.O_RDWR | os.O_CREAT), 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        keys = [key for key in _notification_state() if not key.startswith(sid + ':')]
        write_json(STATE / 'notification-keys.json', keys)

def dispatch_notifications(items):
    """Fan aggregate body-free metadata out to exact existing Desktop sessions."""
    if not isinstance(items, list) or len(items) > 200:
        raise ValueError('invalid_notifications')
    grouped = {}
    for item in items:
        key = _notification_key(item)
        grouped.setdefault(item['session_id'], []).append(key)
    with os.fdopen(private_file(STATE / 'notification.lock', os.O_RDWR | os.O_CREAT), 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        seen = _notification_state()
        seen_set = set(seen)
        for sid, keys in grouped.items():
            new = [key for key in keys if key not in seen_set]
            if not new:
                continue
            try:
                project = route_for(sid)
            except (ValueError, KeyError, OSError, json.JSONDecodeError, discovery.route.RouteError):
                continue
            if discovery.session_deleted(sid, STATE):
                continue
            signal_path = STATE / (sid + '.signal.json')
            if not signal_path.exists():
                continue
            write_json(signal_path, {'session_id': sid, 'count': len(keys), 'sequence': time.time_ns()})
            record(sid, 'notification_dispatched', count=len(keys))
            seen.extend(new)
            seen_set.update(new)
        write_json(STATE / 'notification-keys.json', seen[-4096:])
    return 0

def record(sid, event, **fields):
    with os.fdopen(private_file(STATE / (sid + '.events.jsonl'), os.O_WRONLY | os.O_APPEND | os.O_CREAT), 'w') as stream:
        stream.write(json.dumps({'time_ns': time.time_ns(), 'session_id': sid,
                                'event': event, **fields}) + '\n')

def environment(sid):
    env = os.environ.copy()
    for key in ('CODEX_THREAD_ID', 'CODEX_SESSION_ID', 'CLAUDE_CODE_SESSION_ID', 'TINCAN_SESSION_ID'):
        env.pop(key, None)
    env['TINCAN_SESSION_ID'] = sid
    env['TINCAN_CONFIG'] = str(Path.home() / '.config/tincan/claude-code.json')
    return env

def sync_catalog(sid, project):
    """Publish this verified native route and retire explicit Desktop deletes."""
    if not TINCAN.exists():
        return
    env = environment(sid)
    for row in discovery.known_sessions(STATE):
        old = row['session_id']
        if old == sid or not discovery.session_deleted(old, STATE):
            continue
        result = subprocess.run(
            [str(TINCAN), 'session-retire', '--host', 'claude-code', '--session-id', old],
            env=env, cwd=project, capture_output=True, text=True, timeout=8)
        record(sid, 'catalog_retirement', retired_session_id=old,
               outcome='retired' if result.returncode == 0 else 'deferred')
    result = subprocess.run(
        [str(TINCAN), 'session-discover', '--host', 'claude-code', '--project', str(project)],
        env=env, cwd=project, capture_output=True, text=True, timeout=8)
    record(sid, 'catalog_discovery', outcome='registered' if result.returncode == 0 else 'unavailable')

def process_identity(pid):
    if type(pid) is not int or pid <= 1:
        return None
    result = subprocess.run(['/bin/ps', '-p', str(pid), '-o', 'ppid=', '-o', 'args='],
                            capture_output=True, text=True, timeout=3)
    if result.returncode:
        return None
    parts = result.stdout.strip().split(None, 1)
    if len(parts) != 2 or not parts[0].isdigit():
        return None
    return int(parts[0]), parts[1]

def reclaim_orphan(sid):
    """Migrate a legacy orphan only after checking its exact scoped child."""
    path = STATE / (sid + '.listener.json')
    if path.exists():
        with os.fdopen(private_file(path, os.O_RDONLY)) as stream:
            latest = json.load(stream)
    else:
        # Legacy builds had only an append-only event log. Read a bounded tail.
        path = STATE / (sid + '.events.jsonl')
        if not path.exists():
            return False
        with os.fdopen(private_file(path, os.O_RDONLY)) as stream:
            size = os.fstat(stream.fileno()).st_size
            stream.seek(max(0, size - 131072))
            lines = stream.read().splitlines()
        latest = None
        for line in reversed(lines):
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if value.get('event') == 'listener_started':
                latest = value
                break
        if latest is None:
            return False
    if latest.get('session_id') != sid:
        return False
    pid, child_pid = latest.get('pid'), latest.get('tincan_pid')
    owner, child = process_identity(pid), process_identity(child_pid)
    script = str(Path(__file__).resolve())
    if (not owner or owner[0] != 1 or not owner[1].endswith(' ' + script + ' listen')
            or not child or child[0] != pid
            or not child[1].startswith(str(TINCAN) + ' listen --exec ')
            or not child[1].endswith(' ' + script + ' notice ' + sid)):
        return False
    # Recheck the parent immediately before signaling; never kill a live host's listener.
    if process_identity(pid) != owner or process_identity(child_pid) != child:
        return False
    os.kill(pid, signal.SIGTERM)
    record(sid, 'orphan_listener_reclaimed', pid=pid, tincan_pid=child_pid)
    return True

def run_listener(sid, project):
    """Keep one native listener alive, only while its original host exists."""
    host_pid = os.getppid()
    if host_pid <= 1:
        return
    process = None
    delay = 1
    def stop_listener(_signum, _frame):
        if process is not None and process.poll() is None:
            process.terminate()
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, stop_listener)
    signal.signal(signal.SIGINT, stop_listener)
    notify = shlex.join([sys.executable, str(Path(__file__).resolve()), 'notice', sid])
    while os.getppid() == host_pid:
        started = time.monotonic()
        try:
            process = subprocess.Popen([str(TINCAN), 'listen', '--exec', notify], env=environment(sid),
                                       cwd=project, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            write_json(STATE / (sid + '.listener.json'), {'session_id': sid, 'pid': os.getpid(),
                                                       'tincan_pid': process.pid, 'host_parent_pid': host_pid})
            record(sid, 'listener_started', pid=os.getpid(), tincan_pid=process.pid, host_parent_pid=host_pid)
            while process.poll() is None and os.getppid() == host_pid:
                time.sleep(1)
            if process.poll() is None:
                process.terminate()
            result = process.wait(timeout=5)
            record(sid, 'listener_exited', exit_code=result)
        except OSError:
            record(sid, 'listener_launch_failed')
        except subprocess.TimeoutExpired:
            record(sid, 'listener_stop_timeout')
        finally:
            if process is not None and process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        if os.getppid() != host_pid:
            break
        delay = 1 if time.monotonic() - started >= 30 else min(delay, 60)
        record(sid, 'listener_retry', delay_seconds=delay)
        for _ in range(delay):
            if os.getppid() != host_pid:
                return
            time.sleep(1)
        delay = min(delay * 2, 60)

def receive_lease_seconds(sid):
    """Optional private operator policy, scoped and time-bounded for live QA."""
    path = STATE / (sid + '.receive-policy.json')
    if not path.exists():
        return RECEIVE_LEASE_SECONDS
    with os.fdopen(private_file(path, os.O_RDONLY)) as stream:
        value = json.load(stream)
    if (set(value) != {'session_id', 'lease_seconds', 'expires_at_ns'}
            or value['session_id'] != sid
            or type(value['lease_seconds']) is not int
            or not 10 <= value['lease_seconds'] <= RECEIVE_LEASE_SECONDS
            or type(value['expires_at_ns']) is not int):
        raise ValueError('invalid_receive_policy')
    if value['expires_at_ns'] <= time.time_ns():
        return RECEIVE_LEASE_SECONDS
    return value['lease_seconds']

def notice_sequence(sid, signal_path):
    """Read the current verified notice sequence for this session."""
    with os.fdopen(private_file(signal_path, os.O_RDONLY)) as stream:
        notice = json.load(stream)
    if (notice.get('session_id') != sid
            or type(notice.get('count')) is not int
            or type(notice.get('sequence')) is not int):
        raise ValueError('invalid_notice')
    return notice['sequence'] if notice['count'] > 0 else 0

def unread_notice(sid, signal_path):
    """Return true only for a new, non-empty notice owned by this session."""
    sequence = notice_sequence(sid, signal_path)
    if sequence <= 0:
        return False
    seen_path = STATE / (sid + '.seen.json')
    if not seen_path.exists():
        return True
    with os.fdopen(private_file(seen_path, os.O_RDONLY)) as stream:
        seen = json.load(stream)
    return type(seen.get('sequence')) is int and sequence > seen['sequence']

def wait_and_rewake(sid, project, lease_seconds=RECEIVE_LEASE_SECONDS):
    """One renewable native asyncRewake observer; never claims work."""
    if not 0 < lease_seconds <= RECEIVE_LEASE_SECONDS:
        raise ValueError('invalid_receive_lease')
    with os.fdopen(private_file(STATE / (sid + '.receive.lock'), os.O_RDWR | os.O_CREAT), 'r+') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            record(sid, 'receive_wait_skipped', reason='already_active')
            return 0
        host_pid = os.getppid()
        if host_pid <= 1:
            record(sid, 'receive_wait_cancelled', reason='host_exited')
            return 0
        deadline = time.monotonic() + lease_seconds
        notify = shlex.join([sys.executable, str(Path(__file__).resolve()), 'notice', sid])
        signal_path = STATE / (sid + '.signal.json')
        while True:
            before_sequence = notice_sequence(sid, signal_path)
            process = subprocess.Popen([str(TINCAN), 'listen', '--once', '--exec', notify],
                                       env=environment(sid), cwd=project,
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            record(sid, 'receive_wait_started', host_parent_pid=host_pid,
                   tincan_pid=process.pid, lease_seconds=lease_seconds)
            try:
                while process.poll() is None and os.getppid() == host_pid:
                    if time.monotonic() >= deadline:
                        # Exit before the host's 24h timeout so a real native turn
                        # can finish and rearm Stop. No inbox/task is consumed.
                        if os.getppid() != host_pid:
                            record(sid, 'receive_wait_cancelled', reason='host_exited')
                            return 0
                        record(sid, 'receive_renewal_requested', host_parent_pid=host_pid)
                        print('Tincan receiver lease renewal only. No task has been '
                              'delivered or claimed. Finish this turn normally now '
                              'so the native Stop hook rearms the receiver for this '
                              'same conversation. Do not inspect the inbox, call '
                              'tools, replay work, send messages, change files, '
                              'create chats, or resume CLI.', file=sys.stderr)
                        return 2
                    time.sleep(.25)
                if os.getppid() != host_pid:
                    record(sid, 'receive_wait_cancelled', reason='host_exited')
                    return 0
                result = process.wait(timeout=5)
                if result != 0:
                    record(sid, 'receive_wait_cancelled', reason='listener_failed')
                    return 0
                if notice_sequence(sid, signal_path) > before_sequence and unread_notice(sid, signal_path):
                    return wake_notice(sid, project, signal_path,
                                       {'file_path': str(signal_path), 'event': 'change'}, 'native_wait')
                # Relay restarts and upgrade-only notices can end a one-shot
                # listener without delivering session work. Keep the same hook
                # alive and re-arm it instead of silently losing this receiver.
                record(sid, 'receive_wait_rearmed', reason='no_new_notice')
                if os.getppid() != host_pid:
                    record(sid, 'receive_wait_cancelled', reason='host_exited')
                    return 0
                time.sleep(.25)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)

def wake_notice(sid, project, signal_path, item, surface='file_changed'):
    # Log only enums/booleans: never persist an input path, peer body or exception.
    record(sid, 'wake_hook_invoked', surface=surface,
           file_matches=item.get('file_path') == str(signal_path),
           native_event=item.get('event') if item.get('event') in ('change', 'add', 'unlink') else 'unknown')
    if item.get('file_path') != str(signal_path) or item.get('event') == 'unlink':
        record(sid, 'wake_skipped', reason='wrong_path_or_unlink')
        return 0
    with os.fdopen(private_file(STATE / (sid + '.wake.lock'), os.O_RDWR | os.O_CREAT), 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        with os.fdopen(private_file(signal_path, os.O_RDONLY)) as stream:
            notice = json.load(stream)
        if notice['session_id'] != sid or notice['count'] <= 0:
            record(sid, 'wake_skipped', reason='wrong_owner_or_empty')
            return 0
        seen_path = STATE / (sid + '.seen.json')
        if seen_path.exists():
            with os.fdopen(private_file(seen_path, os.O_RDONLY)) as stream:
                if json.load(stream)['sequence'] >= notice['sequence']:
                    record(sid, 'wake_skipped', reason='already_seen')
                    return 0
        write_json(seen_path, {'sequence': notice['sequence']})
        record(sid, 'desktop_rewake', count=notice['count'], host_parent_pid=os.getppid(), surface=surface)
    print('Agent Tincan has waiting items scoped to this existing conversation. '
          'Its communication project remains ' + json.dumps(str(project)) + '. '
          'Directory navigation does not reassign its owner. Verify each request\'s '
          'project before editing; use needs_input for an unresolved project mismatch. '
          'Call native agent-tincan check_inbox in this session, handle its requests '
          'and replies under this session\'s normal permissions, and reply through '
          'native Tincan. This is a wake notice only; no work has been claimed. '
          'Do not open, create, fork, or CLI-resume any conversation. '
          'Do not replay completed requests or claim another session\'s work.', file=sys.stderr)
    return 2

def main(mode):
    if mode == 'notice':
        sid = sys.argv[2]
        route_for(sid)
        if os.environ.get('TINCAN_SESSION_ID') != sid:
            raise ValueError('wrong_notice_scope')
        count = int(os.environ.get('TINCAN_WAITING', '0'))
        if count <= 0:
            return 0
        state_directory()
        write_json(STATE / (sid + '.signal.json'), {'session_id': sid, 'count': count, 'sequence': time.time_ns()})
        record(sid, 'notice', count=count)
        return 0
    if mode == 'dispatch':
        state_directory()
        raw = os.environ.get('TINCAN_SESSION_NOTIFICATIONS', '[]')
        if len(raw) > 131072:
            raise ValueError('notification_metadata_too_large')
        return dispatch_notifications(json.loads(raw))
    if mode == 'recover':
        item = json.load(sys.stdin)
        if not recovery_allowed(item):
            return 0
        # Guard exclusions before session registration or private state writes.
        item, sid, project = context(mode, item)
        state_directory()
    else:
        state_directory()
        item, sid, project = context(mode)
    signal_path = STATE / (sid + '.signal.json')
    if mode == 'receive':
        return wait_and_rewake(sid, project, receive_lease_seconds(sid))
    if mode == 'prepare':
        sync_catalog(sid, project)
        if not signal_path.exists():
            write_json(signal_path, {'session_id': sid, 'count': 0, 'sequence': 0})
        record(sid, 'watch_registered', host_parent_pid=os.getppid(), hook_event=item['hook_event_name'])
        reset_notification_state(sid)
        if not Path(item['cwd']).resolve(strict=True).is_relative_to(project):
            record(sid, 'cwd_changed_owner_preserved', host_parent_pid=os.getppid())
        if item['hook_event_name'] == 'SessionStart':
            print(json.dumps({'hookSpecificOutput': {'hookEventName': 'SessionStart', 'watchPaths': [str(signal_path)]}}))
        else:
            print(json.dumps({'watchPaths': [str(signal_path)]}))
        return 0
    if mode in ('listen', 'recover'):
        with os.fdopen(private_file(STATE / (sid + '.listen.lock'), os.O_RDWR | os.O_CREAT), 'r+') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                if not reclaim_orphan(sid):
                    return 0
                for _ in range(30):
                    try:
                        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        break
                    except BlockingIOError:
                        time.sleep(.1)
                else:
                    return 0
            run_listener(sid, project)
        return 0
    if mode == 'wake':
        return wake_notice(sid, project, signal_path, item)
    raise ValueError('unknown_mode')

if __name__ == '__main__':
    try:
        result = main(sys.argv[1])
    except (ValueError, KeyError, OSError, json.JSONDecodeError, discovery.route.RouteError) as exc:
        # Malformed events must not awaken a model. Keep failures diagnosable
        # without exposing peer content, paths, credentials or exception text.
        reason = str(exc) if isinstance(exc, discovery.route.RouteError) else 'invalid_or_unavailable_hook_context'
        print('Tincan hook skipped: ' + reason, file=sys.stderr)
        result = 0
    raise SystemExit(result)
