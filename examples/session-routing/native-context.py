#!/usr/bin/env python3
"""Sign actual native hook metadata. Never accept identity from peer text."""
import argparse
import importlib.util
import json
from pathlib import Path
import sys

ROOT = Path.home() / '.local/share/tincan/adapters'
source = ROOT / 'session-routing/project-router.py'
if not source.exists():
    source = Path(__file__).with_name('router.py')
spec = importlib.util.spec_from_file_location('tincan_native_router', source)
router = importlib.util.module_from_spec(spec)
spec.loader.exec_module(router)
router.HOSTS.update({'opencode', 'hermes'})


def context(host, sid, project):
    if host not in {'opencode', 'hermes'}:
        raise ValueError('unsupported_native_host')
    row = router.update(Path.home() / '.config/tincan/extra-session-routes.json', host, host, project, sid)
    # Only metadata is published. A hook cannot send, claim or replay work.
    router.publish(row, Path.home() / '.local/bin/tincan',
                   Path.home() / ('.config/tincan/hermes.json' if host == 'hermes'
                                  else '.config/tincan/opencode.json'))
    return router.token(row, router.DEFAULT_KEY)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', required=True)
    parser.add_argument('--session-id', required=True)
    parser.add_argument('--project', required=True)
    args = parser.parse_args()
    try:
        print(context(args.host, args.session_id, args.project))
    except Exception:
        print('native_tincan_context_unavailable', file=sys.stderr)
        sys.exit(1)
