#!/usr/bin/env python3
"""Operator-configured create-or-resolve adapter; never claims or sends work."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import uuid

spec = importlib.util.spec_from_file_location('session_route', Path(__file__).with_name('session-route.py'))
route = importlib.util.module_from_spec(spec)
spec.loader.exec_module(route)
spec = importlib.util.spec_from_file_location('desktop_sessions', Path(__file__).with_name('desktop-sessions.py'))
discovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(discovery)

def environment():
    allowed = {'HOME', 'USER', 'LOGNAME', 'PATH', 'TMPDIR', 'LANG', 'TERM'}
    env = {k: v for k, v in os.environ.items() if k in allowed}
    env.setdefault('HOME', str(Path.home()))
    env['NO_COLOR'] = '1'
    return env

def resolve_or_create(registry, project, model, claude='claude', timeout=90, *, allow_create=False, state=discovery.DEFAULT_STATE):
    state_directory = Path(state)
    project = route._project_directory(project)
    roots = json.loads(os.environ.get('TINCAN_PROJECT_ROOTS', json.dumps([
        str(Path.home() / 'Workspace/repos'), str(Path.home() / 'Documents/Codex')
    ])))
    allowed = [Path(root).expanduser().resolve(strict=True) for root in roots]
    if model and (not model.startswith("claude-") or len(model) > 128 or any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._' for c in model)):
        raise route.RouteError('explicit_model_required')
    key = hashlib.sha256(str(project).encode()).hexdigest()
    # One lock per canonical project, including while bootstrapping. The
    # registry's own separate lock serializes atomic updates across projects.
    with route._open_registry_directory(registry) as (directory_fd, name):
        with route._registry_lock(directory_fd, name + '.project-' + key):
            state = route._registry_state(directory_fd, name, missing_ok=True)
            if (state and state_directory.exists()
                    and os.access(state_directory, os.W_OK | os.X_OK)):
                retained = [row for row in state['routes']
                            if row['project'] != str(project)
                            or not discovery.session_deleted(row['session_id'], state_directory)]
                if len(retained) != len(state['routes']):
                    state = {'version': state['version'], 'routes': retained}
                    route._atomic_write(directory_fd, name, state)
            matches = [r for r in (state or {'routes': []})['routes'] if r['project'] == str(project)]
            primary = [r for r in matches if r['task'] == 'project']
            if len(matches) > 1 and not primary:
                raise route.RouteError('multiple_project_sessions_require_owner_decision')
            if matches:
                return {'action': 'resolved', **(primary or matches)[0], 'requested_model': model, 'model_verified': False}
            # Discovery contains existing host sessions, never model-created IDs.
            if state_directory.exists():
                known = discovery.candidates(project, state_directory)
                if len(known) > 1:
                    raise route.RouteError('multiple_project_sessions_require_owner_decision')
                if known:
                    bound, _ = route.bind_route(registry, project, 'project', known[0]['session_id'])
                    return {'action': 'discovered', **bound, 'requested_model': model, 'model_verified': False}
            if not allow_create:
                raise route.RouteError('project_session_not_discovered_open_existing_desktop_chat')
            # An existing private operator binding is already an explicit
            # conversation choice, including projects outside bootstrap roots.
            # The roots still restrict every attempt to create a conversation.
            if not any(project == root or root in project.parents for root in allowed):
                raise route.RouteError('project_outside_operator_roots')
            checkpoint = name + '.creation-' + key + '.json'
            try:
                fd = route._open_private_regular(directory_fd, checkpoint, os.O_RDONLY)
            except route.RouteError:
                try:
                    os.stat(checkpoint, dir_fd=directory_fd, follow_symlinks=False)
                except FileNotFoundError:
                    pending = None
                else:
                    raise
            else:
                with os.fdopen(fd) as stream:
                    raw = stream.read(65537)
                    if len(raw) > 65536:
                        raise route.RouteError('creation_checkpoint_too_large')
                    pending = json.loads(raw)
            if pending is not None:
                if not isinstance(pending, dict) or set(pending) != {'phase', 'project', 'model', 'session_id'}:
                    raise route.RouteError('invalid_creation_checkpoint')
                if pending.get('project') != str(project) or pending.get('model') != model:
                    raise route.RouteError('creation_checkpoint_conflict')
                if pending.get('phase') != 'verified':
                    raise route.RouteError('creation_needs_review_no_retry')
                actual_id = route._session_uuid(pending['session_id'])
            else:
                auth = subprocess.run([claude, 'auth', 'status', '--json'], capture_output=True,
                                      text=True, env=environment(), timeout=15, cwd=project)
                if auth.returncode or json.loads(auth.stdout).get('loggedIn') is not True:
                    raise route.RouteError('claude_auth_required')
                actual_id = str(uuid.uuid4())
                pending = {'phase': 'initializing', 'project': str(project), 'model': model, 'session_id': actual_id}
                route._atomic_write(directory_fd, checkpoint, pending)
                try:
                    invocation = [
                        claude, '--print', '--output-format', 'json', '--session-id', actual_id,
                        '--tools', '', '--strict-mcp-config', '--mcp-config',
                        '{"mcpServers":{}}', '--disable-slash-commands', '--max-turns', '1',
                        'Initialize this persistent project conversation only. Do not use tools, '
                        'claim work, edit files or start another agent. State your model briefly.'
                    ]
                    # With no requested model, retain Claude's native configured
                    # default. Never choose a fallback model or alter an owner.
                    if model:
                        invocation[1:1] = ['--model', model]
                    response = subprocess.run(invocation, capture_output=True, text=True, env=environment(), timeout=timeout, cwd=project)
                    if len(response.stdout) > 1048576:
                        raise route.RouteError('bootstrap_result_too_large')
                    result = json.loads(response.stdout)
                    usage = result.get('modelUsage', {})
                    if response.returncode or result.get('is_error') or result.get('session_id') != actual_id or not isinstance(usage, dict) or len(usage) != 1 or (model and set(usage) != {model}):
                        raise route.RouteError('bootstrap_identity_or_model_not_verified')
                except (subprocess.TimeoutExpired, ValueError, route.RouteError):
                    pending['phase'] = 'needs_review'
                    route._atomic_write(directory_fd, checkpoint, pending)
                    raise route.RouteError('creation_needs_review_no_retry') from None
                pending['phase'] = 'verified'
                route._atomic_write(directory_fd, checkpoint, pending)
            bound, _ = route.bind_route(registry, project, 'project', actual_id)
            return {'action': 'created', **bound, 'requested_model': model, 'model_verified': True}

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--registry', type=Path, default=route.DEFAULT_REGISTRY)
    parser.add_argument('--project', type=Path, required=True)
    parser.add_argument('--model', default='', help='optional; otherwise preserve the native Claude default')
    parser.add_argument('--claude-bin', default='claude')
    parser.add_argument('--allow-create', action='store_true', help='explicit operator opt-in; default only reuses existing sessions')
    args = parser.parse_args()
    try:
        print(json.dumps(resolve_or_create(args.registry, args.project, args.model, args.claude_bin, allow_create=args.allow_create)))
    except (route.RouteError, ValueError, OSError, subprocess.TimeoutExpired) as exc:
        print(str(exc) if isinstance(exc, route.RouteError) else 'project_session_unavailable', file=__import__('sys').stderr)
        raise SystemExit(2)
