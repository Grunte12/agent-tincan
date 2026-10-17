#!/usr/bin/env python3
"""Native runtime entrypoint: register actual Muse context, then exec tincan wait.

The owning runtime supplies {sessionId,cwd} on stdin after native resume. This
does not implement a Hatch hook, connect an MSP client, or open a conversation.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import sys

spec = importlib.util.spec_from_file_location('router', Path(__file__).with_name('router.py'))
router = importlib.util.module_from_spec(spec); spec.loader.exec_module(router)

def prepare(event, agent, registry, environment):
    sid, project = router.event_identity('muse', event)
    row = router.update(registry, agent, 'muse', project, sid)
    env = environment.copy()
    # Runtime context, not a previously inherited Codex/Claude conversation.
    env.pop('CODEX_THREAD_ID', None)
    env.pop('CLAUDE_CODE_SESSION_ID', None)
    env['TINCAN_SESSION_ID'] = sid
    # Preserve native Hatch authentication/proxy/config environment untouched.
    return row, env

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--agent', default='linkhub-muse')
    p.add_argument('--registry', type=Path, default=router.DEFAULT_REGISTRY)
    p.add_argument('--tincan', default='tincan')
    p.add_argument('--publish', action='store_true')
    p.add_argument('--config', type=Path)
    p.add_argument('--project-roots', type=Path)
    p.add_argument('--register-only', action='store_true')
    args = p.parse_args()
    try:
        raw = sys.stdin.buffer.read(1_048_577)
        if len(raw) > 1_048_576: raise router.Error('runtime_context_too_large')
        event = json.loads(raw, object_pairs_hook=router.private._duplicate_rejecting_object)
        row, env = prepare(event, args.agent, args.registry, os.environ)
        if args.publish:
            if not args.config: raise router.Error('operator_native_publisher_required')
            router.publish(row, args.tincan, args.config, args.project_roots)
            env['TINCAN_CONFIG'] = str(args.config)
        if args.register_only:
            print(json.dumps(row, sort_keys=True))
            return 0
        os.chdir(event['cwd'])
        os.execvpe(args.tincan, [args.tincan, 'wait'], env)
    except (router.Error, OSError, ValueError, TypeError) as exc:
        print(str(exc) if isinstance(exc, router.Error) else 'muse_runtime_context_unavailable', file=sys.stderr)
        return 2

if __name__ == '__main__':
    raise SystemExit(main())
