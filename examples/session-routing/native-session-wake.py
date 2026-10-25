#!/usr/bin/env python3
"""Wake only registered OpenCode/Hermes native sessions from count-only notices."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path.home() / '.local/share/tincan/adapters'
spec = importlib.util.spec_from_file_location('cursor_wake_support', ROOT / 'cursor-session-wake.py')
support = importlib.util.module_from_spec(spec)
spec.loader.exec_module(support)
spec = importlib.util.spec_from_file_location('native_creation_support', Path(__file__).with_name('project-create.py'))
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
PROMPT = ('Tincan: check_inbox once in this exact session. Process only requests you successfully '
          'claim under existing user permissions; reply through Tincan. Do not create or fork a chat.')


def routes(host):
    registry = native.registry_for(host)
    if not registry.exists():
        return {}
    data = support.private_json(registry)
    native.router.validate(data)
    return {row['session_id']: row['project'] for row in data['sessions']
            if row['agent'] == host and row['host'] == host}


def wake(host, sid, project):
    lock = support.lock_session(sid, Path.home() / ('.local/state/tincan/' + host + '-session-wake'))
    if lock is None:
        return 'already_running'
    try:
        if host == 'opencode':
            info = native.opencode('GET', project, sid)
            if info.get('id') != sid or info.get('directory') != project:
                raise ValueError('native_scope_mismatch')
            statuses = native.opencode('GET', project, action='status')
            if statuses.get(sid, {}).get('type', 'idle') != 'idle':
                return 'already_active'
            native.opencode('POST', project, sid,
                            {'parts': [{'type': 'text', 'text': PROMPT}]}, action='prompt_async')
            return 'accepted'
        env = native.child_env()
        env['TINCAN_CONFIG'] = str(Path.home() / '.config/tincan/hermes.json')
        env['TINCAN_SESSION_ID'] = sid
        # The normal query command preserves approvals; -z enables YOLO upstream.
        result = subprocess.run([str(Path.home() / '.local/bin/hermes'), '--cli', 'chat',
                                 '--resume', sid, '-q', PROMPT], cwd=project, env=env,
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=1800)
        return 'completed' if result.returncode == 0 else 'native_rejected'
    finally:
        os.close(lock)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', choices=['opencode', 'hermes'], required=True)
    args = parser.parse_args()
    known = routes(args.host)
    for sid in support.notified_sessions(os.environ.get('TINCAN_SESSION_NOTIFICATIONS', ''), known):
        print(json.dumps({'host': args.host, 'session_id': sid, 'status': wake(args.host, sid, known[sid])}))


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('native_session_wake_unconfirmed', file=sys.stderr)
        sys.exit(1)
