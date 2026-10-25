#!/usr/bin/env python3
"""Create one native project conversation; never receive or replay task content."""
import fcntl
import hashlib
import http.client
import importlib.util
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import stat
import subprocess
import time
import urllib.parse

HOME = Path.home()
STATE = HOME / '.config/tincan/project-creation'
REGISTRY = HOME / '.config/tincan/host-session-routes.json'
EXTRA_REGISTRY = HOME / '.config/tincan/extra-session-routes.json'
ADAPTERS = HOME / '.local/share/tincan/adapters'
HOSTS = {'claude-code', 'codex', 'cursor', 'opencode', 'hermes'}
ID = re.compile(r'[A-Za-z0-9_-]{1,128}\Z')
router_path = Path(__file__).with_name('project-router.py')
if not router_path.exists():
    router_path = Path(__file__).with_name('router.py')
spec = importlib.util.spec_from_file_location('project_router', router_path)
router = importlib.util.module_from_spec(spec)
spec.loader.exec_module(router)


def save(path, value):
    temp = path.with_name(path.name + '.tmp')
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(value, stream)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temp, path)


def child_env():
    env = os.environ.copy()
    for key in list(env):
        if key.startswith(('TINCAN_', 'CODEX_', 'CLAUDE_CODE_', 'ANTIGRAVITY_', 'HERMES_SESSION_')) or key == 'HERMES_YOLO_MODE':
            env.pop(key, None)
    bins = [HOME / '.local/bin', HOME / '.local/share/mise/installs/node/lts/bin', Path('/opt/homebrew/bin')]
    env['PATH'] = os.pathsep.join([str(p) for p in bins if p.is_dir()] + [env.get('PATH', '/usr/bin:/bin')])
    return env


def native_cli(argv, project, timeout=18):
    result = subprocess.run(argv, cwd=project, env=child_env(), capture_output=True,
                            text=True, timeout=timeout, check=True)
    if len(result.stdout) > 1_048_576:
        raise RuntimeError('native_response_too_large')
    return result.stdout


class RPC:
    def __init__(self, argv, project):
        self.process = subprocess.Popen(argv, cwd=project, env=child_env(), stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.process.stdout, selectors.EVENT_READ)
        self.buffer = b''
        self.sequence = 0

    def send(self, value):
        self.process.stdin.write(json.dumps(value).encode() + b'\n')
        self.process.stdin.flush()

    def call(self, method, params):
        self.sequence += 1
        self.send({'jsonrpc': '2.0', 'id': self.sequence, 'method': method, 'params': params})
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            while b'\n' in self.buffer:
                line, self.buffer = self.buffer.split(b'\n', 1)
                try:
                    value = json.loads(line)
                except ValueError:
                    continue
                if value.get('id') == self.sequence:
                    if value.get('error') is not None:
                        raise RuntimeError('native_rpc_rejected')
                    return value['result']
            if not self.selector.select(max(0, deadline - time.monotonic())):
                break
            chunk = os.read(self.process.stdout.fileno(), 65536)
            if not chunk:
                break
            self.buffer += chunk
            if len(self.buffer) > 1_048_576:
                raise RuntimeError('native_response_too_large')
        raise RuntimeError('native_rpc_unconfirmed')

    def close(self):
        self.selector.close()
        self.process.stdin.close()
        try:
            self.process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            self.process.wait(timeout=2)
        self.process.stdout.close()


def opencode(method, project, sid='', body=None, action=''):
    # This is the already-configured local native server, never a peer URL.
    query = urllib.parse.urlencode({'directory': project})
    suffix = '/' + urllib.parse.quote(sid, safe='') if sid else ''
    if action not in {'', 'status', 'prompt_async'} or (action == 'prompt_async' and not sid):
        raise ValueError('invalid_native_action')
    if action:
        suffix += '/' + action
    connection = http.client.HTTPConnection('127.0.0.1', 4096, timeout=8)
    try:
        connection.request(method, '/session' + suffix + '?' + query,
                           body=json.dumps(body) if body is not None else None,
                           headers={'Content-Type': 'application/json'})
        response = connection.getresponse()
        raw = response.read(1_048_577)
        if not 200 <= response.status < 300 or len(raw) > 1_048_576:
            raise RuntimeError('native_opencode_unavailable')
        return json.loads(raw) if raw else None
    finally:
        connection.close()


def preflight(host, project):
    binaries = {'claude-code': shutil.which('claude', path=child_env()['PATH']), 'codex': shutil.which('codex', path=child_env()['PATH']),
                'cursor': str(HOME / '.local/bin/cursor-agent'),
                'hermes': str(HOME / '.local/bin/hermes')}
    if host == 'opencode':
        opencode('GET', project)
    elif not binaries.get(host) or not os.access(binaries[host], os.X_OK):
        raise RuntimeError('native_executable_unavailable_no_creation')
    if host == 'claude-code':
        auth = json.loads(native_cli([binaries[host], 'auth', 'status', '--json'], project))
        if auth.get('loggedIn') is not True:
            raise RuntimeError('native_login_required_no_creation')


def create_native(host, project, remember=lambda sid: None):
    title = Path(project).name
    if host == 'claude-code':
        path = ADAPTERS / 'claude-code/project-session.py'
        spec = importlib.util.spec_from_file_location('claude_project_create', path)
        adapter = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(adapter)
        result = adapter.resolve_or_create(adapter.route.DEFAULT_REGISTRY, Path(project), '',
                                           shutil.which('claude', path=child_env()['PATH']) or 'claude', timeout=18,
                                           allow_create=True)
        remember(result['session_id'])
        return result['session_id']
    if host == 'cursor':
        result = native_cli([str(HOME / '.local/bin/cursor-agent'), '--workspace', project,
                             'create-chat'], project).strip()
        if not ID.fullmatch(result):
            raise RuntimeError('native_creation_result_unconfirmed')
        remember(result)
        return result
    if host == 'opencode':
        result = opencode('POST', project, body={'title': title})
        sid = result['id']
        remember(sid)
        confirmed = opencode('GET', project, sid)
        if confirmed.get('id') != sid or confirmed.get('directory') != project:
            raise RuntimeError('native_project_identity_mismatch')
        return sid
    if host == 'codex':
        rpc = RPC([shutil.which('codex', path=child_env()['PATH']) or 'codex', 'app-server'], project)
        try:
            rpc.call('initialize', {'clientInfo': {'name': 'tincan-project-create', 'version': '1'}})
            rpc.send({'jsonrpc': '2.0', 'method': 'initialized'})
            result = rpc.call('thread/start', {'cwd': project, 'ephemeral': False})['thread']
            if result.get('cwd') != project or result.get('ephemeral'):
                raise RuntimeError('native_project_identity_mismatch')
            sid = result['id']
            remember(sid)
            rpc.call('thread/name/set', {'threadId': sid, 'name': title})
            confirmed = rpc.call('thread/read', {'threadId': sid, 'includeTurns': False})['thread']
            if confirmed.get('id') != sid or confirmed.get('cwd') != project or confirmed.get('ephemeral'):
                raise RuntimeError('native_project_identity_mismatch')
            return sid
        finally:
            rpc.close()
    if host == 'hermes':
        rpc = RPC([str(HOME / '.local/bin/hermes'), '--run-module', 'tui_gateway.entry'], project)
        try:
            result = rpc.call('session.create', {'cwd': project, 'title': title,
                                                'idempotency_key': 'tincan:' + project})
            if result.get('info', {}).get('cwd') != project:
                raise RuntimeError('native_project_identity_mismatch')
            sid = result['session_id']
            remember(result['stored_session_id'])
            titled = rpc.call('session.title', {'session_id': sid, 'title': title})
            if titled.get('pending') is not False:
                raise RuntimeError('native_session_not_persisted')
            return result['stored_session_id']
        finally:
            rpc.close()
    raise RuntimeError('unsupported_operator_native_host')


def existing(request):
    registry_path = registry_for(request['host'])
    if not registry_path.exists():
        return None
    with router.private._open_registry_directory(registry_path) as (fd, name):
        with router.private._registry_lock(fd, name):
            registry = router.load(fd, name)
    for key in ('owners', 'sessions'):
        rows = [r for r in registry.get(key, []) if all(r.get(k) == request[k]
                for k in ('agent', 'host', 'project'))]
        ids = {r['session_id'] for r in rows}
        if len(ids) > 1:
            raise RuntimeError('ambiguous_native_owner_no_creation')
        if rows:
            return {k: rows[0][k] for k in ('agent', 'host', 'project', 'session_id')}
    return None


def register_local(binding):
    router.update(registry_for(binding['host']), binding['agent'], binding['host'], binding['project'],
                  binding['session_id'], bind=True)


def registry_for(host):
    # Existing trusted hooks validate the old host set; keep their file/hash
    # unchanged while adding hosts through the shared relay catalog.
    return EXTRA_REGISTRY if host in {'opencode', 'hermes'} else REGISTRY


def ensure(request):
    if set(request) != {'agent', 'host', 'project'} or request['host'] not in HOSTS:
        raise RuntimeError('unsupported_operator_native_host')
    expected_agent = request['host']
    if request['agent'] != expected_agent:
        raise RuntimeError('operator_agent_host_mismatch')
    path = Path(request['project'])
    if not path.is_absolute() or str(path.resolve(strict=True)) != str(path) or not path.is_dir():
        raise RuntimeError('canonical_existing_project_required')
    roots = (HOME / 'Workspace/repos', HOME / 'Documents/Codex')
    if not any(path == root or root in path.parents for root in roots):
        raise RuntimeError('project_outside_operator_roots')
    STATE.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = STATE.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise RuntimeError('private_creation_state_required')
    key = hashlib.sha256((request['agent'] + '\0' + request['host'] + '\0' + str(path)).encode()).hexdigest()
    fd = os.open(STATE / (key + '.lock'), os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        found = existing(request)
        if found:
            return found
        receipt = STATE / (key + '.json')
        if receipt.exists():
            with router.private._open_registry_directory(receipt) as (directory, _):
                descriptor = router.private._open_private_regular(directory, receipt.name, os.O_RDONLY)
                with os.fdopen(descriptor) as stream:
                    raw = stream.read(8193)
            if len(raw) > 8192:
                raise RuntimeError('invalid_creation_receipt')
            previous = json.loads(raw)
            if previous.get('phase') != 'verified' or previous.get('request') != request:
                raise RuntimeError('prior_native_creation_uncertain_do_not_retry')
            if previous.get('binding') != {**request, 'session_id': previous.get('binding', {}).get('session_id')} or not ID.fullmatch(previous['binding'].get('session_id', '')):
                raise RuntimeError('invalid_creation_receipt')
            register_local(previous['binding'])
            return previous['binding']
        preflight(request['host'], str(path))
        save(receipt, {'phase': 'started', 'request': request})
        def remember(sid):
            if not isinstance(sid, str) or not ID.fullmatch(sid):
                raise RuntimeError('native_creation_result_unconfirmed')
            save(receipt, {'phase': 'native_id_received', 'request': request,
                           'binding': {**request, 'session_id': sid}})
        sid = create_native(request['host'], str(path), remember)
        if not isinstance(sid, str) or not ID.fullmatch(sid):
            raise RuntimeError('native_creation_result_unconfirmed')
        binding = {**request, 'session_id': sid}
        save(receipt, {'phase': 'verified', 'request': request, 'binding': binding})
        register_local(binding)
        return binding
    finally:
        os.close(fd)
