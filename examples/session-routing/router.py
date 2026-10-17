#!/usr/bin/env python3
"""Existing-session routing and optional native metadata publication. No model launch."""
import argparse
import base64
import hashlib
import hmac
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time

private_source = Path(__file__).parent.parent / 'claude-code/session-route.py'
if not private_source.is_file():
    private_source = Path(__file__).with_name('session-route.py')
spec = importlib.util.spec_from_file_location('private_files', private_source)
private = importlib.util.module_from_spec(spec)
spec.loader.exec_module(private)
Error = private.RouteError
HOSTS = {'codex', 'cursor', 'antigravity', 'muse'}
DEFAULT_REGISTRY = Path.home() / '.config/tincan/host-session-routes.json'
DEFAULT_KEY = Path.home() / '.config/tincan/host-session-key'

def identity(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', value):
        raise Error('invalid_identity')
    return value

def project(value):
    if not isinstance(value, str) or not Path(value).is_absolute():
        raise Error('absolute_project_required')
    return str(private._project_directory(Path(value)))

def validate(state):
    if not isinstance(state, dict) or set(state) != {'version', 'owners', 'sessions', 'preserved'} or type(state['version']) is not int or state['version'] != 1:
        raise Error('invalid_host_registry')
    for kind in ('owners', 'sessions'):
        rows = state[kind]
        if not isinstance(rows, list) or len(rows) > 4096:
            raise Error('host_registry_limit')
        seen = set()
        for row in rows:
            if not isinstance(row, dict) or set(row) != {'agent', 'host', 'project', 'session_id'}:
                raise Error('invalid_host_registry')
            identity(row['agent']); identity(row['session_id'])
            if row['host'] not in HOSTS or not isinstance(row['project'], str) or not Path(row['project']).is_absolute() or os.path.normpath(row['project']) != row['project'] or '\x00' in row['project']:
                raise Error('invalid_host_registry')
            key = (row['agent'], row['host'], row['project'] if kind == 'owners' else row['session_id'])
            if key in seen:
                raise Error('duplicate_host_route')
            seen.add(key)
    if not isinstance(state['preserved'], list) or len(state['preserved']) > 4096:
        raise Error('host_registry_limit')
    seen = set()
    for row in state['preserved']:
        if not isinstance(row, dict) or set(row) != {'agent', 'host', 'session_id'}:
            raise Error('invalid_preserved_owner')
        identity(row['agent']); identity(row['session_id'])
        key = (row['agent'], row['host'])
        if row['host'] not in HOSTS or key in seen:
            raise Error('invalid_preserved_owner')
        seen.add(key)
    return state

def load(fd, name):
    try:
        file = private._open_private_regular(fd, name, os.O_RDONLY)
    except Error:
        try:
            os.stat(name, dir_fd=fd, follow_symlinks=False)
        except FileNotFoundError:
            return {'version': 1, 'owners': [], 'sessions': [], 'preserved': []}
        raise
    with os.fdopen(file, 'rb') as stream:
        raw = stream.read(private.MAX_REGISTRY_BYTES + 1)
    if len(raw) > private.MAX_REGISTRY_BYTES:
        raise Error('registry_too_large')
    try:
        return validate(json.loads(raw, object_pairs_hook=private._duplicate_rejecting_object))
    except (ValueError, UnicodeError):
        raise Error('invalid_host_registry') from None

def update(registry, agent, host, path, sid=None, *, bind=False):
    identity(agent)
    if host not in HOSTS:
        raise Error('unsupported_host')
    path = project(path)
    if sid is not None:
        sid = identity(sid)
    with private._open_registry_directory(Path(registry)) as (fd, name):
        with private._registry_lock(fd, name):
            state = load(fd, name)
            changed = False
            if sid is not None:
                previous = next((r for r in state['sessions'] if (r['agent'], r['host'], r['session_id']) == (agent, host, sid)), None)
                # Cwd/workspace changes do not reassign a conversation's communication project.
                row = previous or {'agent': agent, 'host': host, 'project': path, 'session_id': sid}
                if previous is None:
                    state['sessions'].append(row); changed = True
                path = row['project']
            owner = next((r for r in state['owners'] if (r['agent'], r['host'], r['project']) == (agent, host, path)), None)
            preserved = next((r for r in state['preserved'] if (r['agent'], r['host']) == (agent, host)), None)
            if preserved and sid == preserved['session_id']:
                # Migration only. Retain the historical owner when its native
                # event arrives; never infer its project from a new chat.
                if owner and owner['session_id'] != sid:
                    raise Error('owner_conflict_immutable')
                if not owner:
                    owner = dict(row)
                    state['owners'].append(owner); changed = True
            if bind:
                if owner and owner['session_id'] != sid:
                    raise Error('owner_conflict_immutable')
                if not owner:
                    owner = dict(row)
                    state['owners'].append(owner); changed = True
            if sid is None and not owner:
                if preserved and not any((r['agent'], r['host'], r['session_id']) == (agent, host, preserved['session_id']) for r in state['sessions']):
                    raise Error('preserved_owner_needs_native_discovery')
                candidates = [r for r in state['sessions'] if (r['agent'], r['host'], r['project']) == (agent, host, path)]
                if len(candidates) != 1:
                    raise Error('owner_ambiguous' if candidates else 'existing_session_not_discovered')
                owner = dict(candidates[0])
                state['owners'].append(owner); changed = True
            validate(state)
            if changed:
                private._atomic_write(fd, name, state)
            return dict(row if sid is not None else owner)

def key_bytes(path, *, create=False):
    with private._open_registry_directory(Path(path)) as (fd, name):
        if create:
            with private._registry_lock(fd, name):
                try:
                    os.stat(name, dir_fd=fd, follow_symlinks=False)
                except FileNotFoundError:
                    key = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=fd)
                    with os.fdopen(key, 'wb') as stream:
                        stream.write(secrets.token_bytes(32)); stream.flush(); os.fsync(stream.fileno())
        key = private._open_private_regular(fd, name, os.O_RDONLY, 'unsafe_host_key')
        with os.fdopen(key, 'rb') as stream:
            value = stream.read(33)
        if len(value) != 32:
            raise Error('invalid_host_key')
        return value

def token(row, key, now=None):
    claim = {'version': 1, **row, 'expires': int(time.time() if now is None else now) + 86400}
    payload = base64.urlsafe_b64encode(json.dumps(claim, sort_keys=True, separators=(',', ':')).encode()).rstrip(b'=')
    signature = hmac.new(key_bytes(key), payload, hashlib.sha256).digest()
    return payload.decode() + '.' + base64.urlsafe_b64encode(signature).rstrip(b'=').decode()

def verify(value, key, agent, host, now=None):
    if not isinstance(value, str) or len(value) > 8192:
        raise Error('invalid_host_context')
    try:
        payload, signature = value.split('.')
        actual = base64.urlsafe_b64decode(signature + '=' * (-len(signature) % 4))
        if base64.urlsafe_b64encode(actual).rstrip(b'=').decode() != signature: raise ValueError()
        expected = hmac.new(key_bytes(key), payload.encode(), hashlib.sha256).digest()
        if not hmac.compare_digest(actual, expected):
            raise ValueError()
        decoded = base64.urlsafe_b64decode(payload + '=' * (-len(payload) % 4))
        if base64.urlsafe_b64encode(decoded).rstrip(b'=').decode() != payload: raise ValueError()
        row = json.loads(decoded, object_pairs_hook=private._duplicate_rejecting_object)
        current = time.time() if now is None else now
        if set(row) != {'version', 'agent', 'host', 'project', 'session_id', 'expires'} or type(row['version']) is not int or row['version'] != 1 or row['agent'] != agent or row['host'] != host or type(row['expires']) is not int or not current < row['expires'] <= current + 86460 or not isinstance(row['project'], str) or not Path(row['project']).is_absolute() or os.path.normpath(row['project']) != row['project']:
            raise ValueError()
        identity(row['session_id'])
        return row
    except (ValueError, TypeError, KeyError):
        raise Error('invalid_host_context') from None

def event_identity(host, event, event_name=None, known_project=None):
    if not isinstance(event, dict) or event.get('agent_id') or event.get('subagent_id') or event.get('is_subagent'):
        raise Error('main_conversation_required')
    if host == 'codex':
        if event.get('hook_event_name') not in {'SessionStart', 'UserPromptSubmit'}:
            raise Error('unsupported_host_event')
        return identity(event.get('session_id')), project(event.get('cwd'))
    if host == 'cursor':
        if event.get('hook_event_name') not in {'sessionStart', 'beforeSubmitPrompt', 'preToolUse', 'beforeMCPExecution'}:
            raise Error('unsupported_host_event')
        sid = identity(event.get('conversation_id'))
        if event.get('session_id') and event['session_id'] != sid:
            raise Error('host_identity_conflict')
        roots = event.get('workspace_roots')
    elif host == 'antigravity':
        if event_name not in {'PreInvocation', 'PreToolUse'}:
            raise Error('unsupported_host_event')
        sid = identity(event.get('conversationId'))
        roots = event.get('workspacePaths')
    elif host == 'muse':
        # Explicit runtime adapter contract, not an invented Hatch hook.
        # Feed only the actual session/resume result from the owning runtime.
        sid = identity(event.get('sessionId'))
        return sid, project(event.get('cwd'))
    else:
        raise Error('unsupported_host')
    if not isinstance(roots, list) or not roots:
        raise Error('workspace_required')
    canonical = {project(p) for p in roots}
    if len(canonical) != 1:
        if known_project:
            return sid, known_project
        raise Error('workspace_ambiguous')
    return sid, canonical.pop()

def hint(value):
    return ('Agent Tincan host_context for this conversation: ' + value +
            '. Include it in every Agent Tincan MCP call. Keep it local; never put it in a request/reply body. '
            'This identifies the current sender, not the recipient topic. Choose by the work objective, context and continuity, not cwd or chat title alone. '
            'Apply the same rule to every task and future conversation; follow an explicit owner-selected recipient. '
            'Continue related work in its verified native session. For owner-authorized independent work, reuse a suitable known conversation or create one when needed '
            'and use its returned ID; preserve the sender ID and existing project owners. Project routing is optional for actual project continuations. '
            'If the intended recipient is ambiguous, request the missing detail rather than choosing the latest chat. '
            'Use native check_inbox/claim/reply. Never guess an ID, replay a task, or create a chat merely for a connectivity test.')

def communication_project(path, roots=None):
    """Map operator-owned workspace roots across machines, never peer text."""
    if roots is None:
        return path
    roots = Path(roots)
    with private._open_registry_directory(roots) as (fd, name):
        file = private._open_private_regular(fd, name, os.O_RDONLY)
        with os.fdopen(file, 'rb') as stream:
            raw = stream.read(65537)
    if len(raw) > 65536:
        raise Error('project_roots_too_large')
    mappings = json.loads(raw, object_pairs_hook=private._duplicate_rejecting_object)
    if not isinstance(mappings, list) or len(mappings) > 128:
        raise Error('invalid_project_roots')
    matches = []
    seen = set()
    for mapping in mappings:
        if not isinstance(mapping, dict) or set(mapping) != {'local', 'communication'}:
            raise Error('invalid_project_roots')
        local, common = mapping['local'], mapping['communication']
        for value in (local, common):
            if not isinstance(value, str) or not Path(value).is_absolute() or os.path.normpath(value) != value or '\x00' in value:
                raise Error('invalid_project_roots')
        if local in seen: raise Error('duplicate_project_root')
        seen.add(local)
        candidate = Path(path)
        if candidate == Path(local) or Path(local) in candidate.parents:
            matches.append((len(Path(local).parts), str(Path(common) / candidate.relative_to(local))))
    if not matches:
        raise Error('communication_project_not_mapped')
    return max(matches)[1]

def publish(row, tincan, config, roots=None):
    common = communication_project(row['project'], roots)
    env = os.environ.copy()
    env['TINCAN_SESSION_ID'] = row['session_id']
    env['TINCAN_CONFIG'] = str(config)
    env.pop('CODEX_THREAD_ID', None)
    env.pop('CLAUDE_CODE_SESSION_ID', None)
    result = subprocess.run([str(tincan), 'session-discover', '--host', row['host'], '--project', common],
                            env=env, capture_output=True, text=True, timeout=5)
    if result.returncode:
        raise Error('relay_session_discovery_unavailable')
    if len(result.stdout) > 65536: raise Error('invalid_relay_discovery')
    binding = json.loads(result.stdout)
    if not isinstance(binding, dict) or binding.get('agent') != row['agent'] or binding.get('host') != row['host'] or binding.get('session_id') != row['session_id'] or binding.get('project') != common:
        raise Error('relay_discovery_identity_mismatch')
    return binding

def agy_tincan_arguments(event):
    call = event.get('toolCall', {})
    name = call.get('name', '')
    args = call.get('args', {})
    if name == 'call_mcp_tool':
        if not isinstance(args, dict) or args.get('ServerName') != 'agent-tincan':
            return None
        args = args.get('Arguments')
        if isinstance(args, str):
            args = json.loads(args, object_pairs_hook=private._duplicate_rejecting_object)
    elif not re.search(r'agent[-_]tincan', name):
        return None
    if not isinstance(args, dict):
        raise Error('invalid_tool_arguments')
    return args

def hook(host, agent, event, registry, key, event_name=None):
    cursor_generic_mcp = False
    # A routing hook must never require a Tincan identity/key for another MCP
    # server. In particular Cursor's fail-closed gate observes all MCP calls.
    if isinstance(event, dict) and host == 'cursor':
        kind = event.get('hook_event_name')
        if kind == 'beforeMCPExecution' and event.get('mcp_server_name') != 'agent-tincan':
            return {'permission': 'allow'}
        if kind == 'preToolUse' and not re.search(r'agent[-_]tincan', event.get('tool_name', '')) and event.get('mcp_server_name') != 'agent-tincan':
            # Cursor CLI emits MCP:<bare tool name> with no server identity.
            # Give only this chat's model context; never add our signed token
            # to an unidentified server's arguments. The server-specific
            # beforeMCPExecution gate still checks the actual caller identity.
            cursor_generic_mcp = event.get('tool_name', '').startswith('MCP:')
            if not cursor_generic_mcp:
                return {'permission': 'allow'}
    if isinstance(event, dict) and host == 'antigravity' and event_name == 'PreToolUse' and agy_tincan_arguments(event) is None:
        return {'decision': 'deny_unless_prior_grant'}
    try:
        known_project = None
        if isinstance(event, dict):
            sid = event.get({'codex': 'session_id', 'cursor': 'conversation_id', 'antigravity': 'conversationId', 'muse': 'sessionId'}.get(host))
            with private._open_registry_directory(Path(registry)) as (fd, name):
                state = load(fd, name)
                existing = next((r for r in state['sessions'] if (r['agent'], r['host'], r['session_id']) == (agent, host, sid)), None)
                if existing:
                    known_project = existing['project']
        sid, path = event_identity(host, event, event_name, known_project)
        row = update(registry, agent, host, path, sid)
        value = token(row, key)
    except (Error, OSError, ValueError):
        if cursor_generic_mcp:
            # An unidentified MCP server must not depend on Tincan's files.
            return {'permission': 'allow'}
        raise
    if host == 'cursor':
        kind = event['hook_event_name']
        if kind == 'beforeMCPExecution':
            if event.get('mcp_server_name') != 'agent-tincan':
                return {'permission': 'allow'}
            args = event.get('tool_input')
            if isinstance(args, str):
                args = json.loads(args)
            try:
                claims = verify(args.get('host_context') if isinstance(args, dict) else None, key, agent, host)
                if claims['session_id'] != sid or claims['project'] != row['project']:
                    raise Error('host_identity_conflict')
            except Error:
                return {'permission': 'deny', 'agent_message': hint(value)}
            return {'permission': 'allow'}
        if kind == 'preToolUse':
            name = event.get('tool_name', '')
            if cursor_generic_mcp:
                return {'permission': 'allow', 'additional_context': hint(value)}
            args = event.get('tool_input')
            if not isinstance(args, dict):
                raise Error('invalid_tool_arguments')
            # Native input rewrite keeps the actual chat scope off model guesses.
            return {'permission': 'allow', 'updated_input': {**args, 'host_context': value}}
        return {'additional_context': hint(value)} if kind == 'sessionStart' else {'continue': True}
    if host == 'antigravity':
        if event_name == 'PreToolUse':
            supplied = agy_tincan_arguments(event).get('host_context')
            try:
                claims = verify(supplied, key, agent, host)
                if claims['session_id'] != sid or claims['project'] != row['project']:
                    raise Error('host_identity_conflict')
            except Error:
                return {'decision': 'deny', 'reason': hint(value)}
            # Keep the host's existing grants. Never grant new tool permissions.
            return {'decision': 'deny_unless_prior_grant'}
        return {'injectSteps': [{'ephemeralMessage': hint(value)}]}
    return {'host_context': value} if host == 'muse' else {}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['hook', 'resolve', 'bind', 'init-key'])
    parser.add_argument('--agent')
    parser.add_argument('--host', choices=sorted(HOSTS))
    parser.add_argument('--project')
    parser.add_argument('--session-id')
    parser.add_argument('--model', default='') # accepted for native project-helper compatibility; never changes a model
    parser.add_argument('--event')
    parser.add_argument('--advisory', action='store_true', help='discovery only: never block an ordinary human prompt')
    parser.add_argument('--publish', action='store_true', help='register metadata on the native relay; no task, claim or wake')
    parser.add_argument('--tincan-bin', type=Path)
    parser.add_argument('--config', type=Path)
    parser.add_argument('--project-roots', type=Path)
    parser.add_argument('--registry', type=Path, default=DEFAULT_REGISTRY)
    parser.add_argument('--key', type=Path, default=DEFAULT_KEY)
    args = parser.parse_args()
    try:
        if args.action == 'init-key':
            key_bytes(args.key, create=True); result = {'initialized': True}
        elif args.action == 'hook':
            raw = sys.stdin.buffer.read(1_048_577)
            if len(raw) > 1_048_576: raise Error('hook_payload_too_large')
            event = json.loads(raw, object_pairs_hook=private._duplicate_rejecting_object)
            result = hook(args.host, args.agent, event, args.registry, args.key, args.event)
            cursor_publication = args.host == 'cursor' and (
                event.get('hook_event_name') in {'sessionStart', 'beforeSubmitPrompt'} or
                (event.get('hook_event_name') == 'beforeMCPExecution' and event.get('mcp_server_name') == 'agent-tincan'))
            if args.publish and ((args.host == 'codex') or cursor_publication or (args.host == 'antigravity' and args.event == 'PreInvocation') or args.host == 'muse'):
                if not args.tincan_bin or not args.config: raise Error('operator_native_publisher_required')
                sid = event.get({'codex': 'session_id', 'cursor': 'conversation_id', 'antigravity': 'conversationId', 'muse': 'sessionId'}[args.host])
                with private._open_registry_directory(args.registry) as (fd, name):
                    row = next((r for r in load(fd, name)['sessions'] if (r['agent'], r['host'], r['session_id']) == (args.agent, args.host, sid)), None)
                if row is None: raise Error('native_session_not_registered')
                try:
                    publish(row, args.tincan_bin, args.config, args.project_roots)
                except (Error, OSError, ValueError, subprocess.TimeoutExpired):
                    # Human work/context remains available. Missing central
                    # discovery still fails closed at project resolution.
                    print('relay_session_discovery_unavailable', file=sys.stderr)
        else:
            if args.action == 'bind' and not args.session_id: raise Error('existing_session_required')
            result = update(args.registry, args.agent, args.host, args.project, args.session_id if args.action == 'bind' else None, bind=args.action == 'bind')
        print(json.dumps(result, sort_keys=True))
    except (Error, ValueError, TypeError, OSError, subprocess.TimeoutExpired) as exc:
        print(str(exc) if isinstance(exc, Error) else 'host_routing_unavailable', file=sys.stderr)
        if args.advisory and args.action == 'hook':
            print(json.dumps({'continue': True} if args.host == 'cursor' else {}))
            return 0
        return 2
    return 0

if __name__ == '__main__':
    raise SystemExit(main())
