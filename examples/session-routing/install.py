#!/usr/bin/env python3
"""Install native existing-session adapters without starting/restarting any host."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import tempfile
import tomllib

HERE = Path(__file__).resolve().parent
HOST_AGENTS = {'codex': 'codex', 'cursor': 'cursor', 'antigravity': 'antigravity', 'muse': 'linkhub-muse'}

def hooks(original, host, command):
    result = json.loads(json.dumps(original))
    adapter_names = ('router.py', 'router-intent.py')
    if host == 'antigravity':
        name = 'tincan-native-session-routing'
        if name in result and not any(adapter in json.dumps(result[name]) for adapter in adapter_names):
            raise ValueError('existing_hook_name_conflict')
        result[name] = {
            'PreInvocation': [{'type': 'command', 'command': command + ' --event PreInvocation', 'timeout': 10}],
            'PreToolUse': [{'matcher': '(^|[_:.-])agent[-_]tincan([_:.-]|$)|^call_mcp_tool$', 'hooks': [
                {'type': 'command', 'command': command + ' --event PreToolUse', 'timeout': 10}]}],
        }
        return result
    configured = result.setdefault('hooks', {})
    if host == 'cursor':
        result.setdefault('version', 1)
        additions = {
            'sessionStart': [{'command': command + ' --advisory', 'timeout': 10}],
            'beforeSubmitPrompt': [{'command': command + ' --advisory', 'timeout': 10}],
            'preToolUse': [{'command': command, 'timeout': 10, 'matcher': 'MCP:.*', 'failClosed': True}],
            'beforeMCPExecution': [{'command': command, 'timeout': 10, 'failClosed': True}],
        }
    elif host == 'codex':
        additions = {'SessionStart': [{'matcher': 'startup|resume', 'hooks': [
            {'type': 'command', 'command': command + ' --advisory', 'timeout': 10}]}],
            'UserPromptSubmit': [{'hooks': [{'type': 'command', 'command': command + ' --advisory', 'timeout': 10}]}]}
    else:
        raise ValueError('unsupported_host')
    for event, rows in additions.items():
        existing = configured.setdefault(event, [])
        # Replace this adapter only; retain every unrelated native hook.
        existing[:] = [row for row in existing if not (any(adapter in json.dumps(row) for adapter in adapter_names) and '--host ' + host in json.dumps(row))]
        existing.extend(rows)
    return result

def toml_env(text, changes, binary):
    parsed = tomllib.loads(text)
    if 'agent-tincan' not in parsed.get('mcp_servers', {}):
        raise ValueError('codex_native_mcp_missing')
    section = None
    seen = set()
    lines = []
    for line in text.splitlines():
        if line.strip().startswith('['):
            if section == 'mcp_servers.agent-tincan.env':
                lines.extend(f'{k} = {json.dumps(v)}' for k,v in changes.items() if k not in seen)
            section = line.strip().strip('[]').replace('"', '')
        match = re.match(r'^\s*([A-Za-z0-9_-]+)\s*=', line)
        if section == 'mcp_servers.agent-tincan' and match and match[1] == 'command':
            line = 'command = ' + json.dumps(str(binary))
        elif section == 'mcp_servers.agent-tincan.env' and match and match[1] in changes:
            line = f'{match[1]} = {json.dumps(changes[match[1]])}'
            seen.add(match[1])
        lines.append(line)
    if section == 'mcp_servers.agent-tincan.env':
        lines.extend(f'{k} = {json.dumps(v)}' for k,v in changes.items() if k not in seen)
    elif 'env' not in parsed['mcp_servers']['agent-tincan']:
        lines.extend(['', '[mcp_servers.agent-tincan.env]'] + [f'{k} = {json.dumps(v)}' for k,v in changes.items()])
    result = '\n'.join(lines) + '\n'
    expected = tomllib.loads(result)['mcp_servers']['agent-tincan']
    if expected['command'] != str(binary) or any(expected.get('env', {}).get(k) != v for k,v in changes.items()):
        raise ValueError('unsupported_codex_config_shape')
    return result

def plan(home, python, binary_source, preservation=None, project_roots=None):
    python = Path(python)
    if not python.is_absolute() or not python.is_file() or not os.access(python, os.X_OK):
        raise ValueError('existing_executable_python_required')
    python = python.resolve()
    home = Path(home).resolve()
    dest = home / '.local/share/tincan/adapters/session-routing'
    binary = home / '.local/bin/tincan-session-routing-5'
    key = home / '.config/tincan/host-session-key'
    registry = home / '.config/tincan/host-session-routes.json'
    hostmap = json.dumps({agent: host for host,agent in HOST_AGENTS.items()}, sort_keys=True, separators=(',', ':'))
    common = {'TINCAN_SESSION_ROUTER': str(dest / 'router.py'), 'TINCAN_SESSION_REGISTRY': str(registry),
              'TINCAN_SESSION_HOSTS': hostmap, 'TINCAN_PROJECT_PYTHON': str(python)}
    writes = {}
    native_configs = {}
    for name in ('router.py', 'muse-wait.py'):
        writes[dest / name] = ((HERE / name).read_bytes(), 0o755)
    writes[dest / 'session-route.py'] = ((HERE.parent / 'claude-code/session-route.py').read_bytes(), 0o755)
    writes[binary] = (Path(binary_source).read_bytes(), 0o755)
    for host in ('cursor', 'antigravity'):
        config = home / ('.cursor/mcp.json' if host == 'cursor' else '.gemini/config/mcp_config.json')
        data = json.loads(config.read_text())
        server = data.get('mcpServers', {}).get('agent-tincan')
        if not isinstance(server, dict) or server.get('args') != ['mcp']:
            raise ValueError('existing_native_mcp_required')
        server['command'] = str(binary)
        native_configs[host] = server.get('env', {}).get('TINCAN_CONFIG', str(home / f'.config/tincan/{host}.json'))
        server.setdefault('env', {}).update({**common, 'TINCAN_HOST_KIND': host, 'TINCAN_HOST_KEY': str(key), 'TINCAN_SESSION_ID': ''})
        writes[config] = ((json.dumps(data, indent=2) + '\n').encode(), 0o600)
    config = home / '.codex/config.toml'
    native_configs['codex'] = tomllib.loads(config.read_text())['mcp_servers']['agent-tincan'].get('env', {}).get('TINCAN_CONFIG', str(home / '.config/tincan/codex.json'))
    writes[config] = (toml_env(config.read_text(), {**common, 'TINCAN_HOST_KIND': 'codex', 'TINCAN_SESSION_ID': ''}, binary).encode(), 0o600)
    for host, relative in [('codex', '.codex/hooks.json'), ('cursor', '.cursor/hooks.json'), ('antigravity', '.gemini/config/hooks.json')]:
        file = home / relative
        data = json.loads(file.read_text()) if file.exists() else {}
        command = shlex.join([str(python), str(dest / 'router.py'), 'hook', '--host', host, '--agent', HOST_AGENTS[host], '--registry', str(registry), '--key', str(key)])
        native_config = Path(native_configs[host])
        if not native_config.is_absolute(): raise ValueError('absolute_native_config_required')
        command += ' ' + shlex.join(['--publish', '--tincan-bin', str(binary), '--config', str(native_config)])
        if project_roots: command += ' ' + shlex.join(['--project-roots', str(project_roots)])
        writes[file] = ((json.dumps(hooks(data, host, command), indent=2) + '\n').encode(), 0o600)
    # Preserve UUID pointers from the prior verified receipt report. No chat or
    # transcript is opened; project assignment waits for the genuine old host
    # event. A new chat cannot replace an undiscovered preserved owner.
    if not registry.exists():
        preserved = []
        if preservation:
            for row in preservation.get('tests', []):
                host = {agent: h for h,agent in HOST_AGENTS.items()}.get(row.get('agent'))
                if host and all(row.get(k) for k in ('passed', 'claim_scope_verified', 'reply_scope_verified', 'owner_receipt_verified')) and row.get('recipient_session'):
                    preserved.append({'agent': row['agent'], 'host': host, 'session_id': row['recipient_session']})
        state = {'version': 1, 'owners': [], 'sessions': [], 'preserved': preserved}
        spec = importlib.util.spec_from_file_location('router', HERE / 'router.py')
        router = importlib.util.module_from_spec(spec); spec.loader.exec_module(router)
        router.validate(state)
        writes[registry] = ((json.dumps(state, indent=2) + '\n').encode(), 0o600)
    return writes, key

def apply(writes, backup):
    for path in writes:
        if path.is_symlink() or path.parent.resolve() != path.parent:
            raise ValueError('refusing_symlink_target')
        if path.exists() and not stat.S_ISREG(path.stat().st_mode):
            raise ValueError('regular_install_target_required')
    backup = Path(backup)
    backup.mkdir(parents=True, mode=0o700, exist_ok=True)
    backup.chmod(0o700)
    receipts = []
    for index,(path,(payload,mode)) in enumerate(writes.items()):
        if path.is_symlink():
            raise ValueError('refusing_symlink_target')
        old = path.read_bytes() if path.exists() else None
        if old == payload:
            continue
        if old is not None:
            saved = backup / f'{index:02d}-{path.name}'
            if saved.exists():
                raise ValueError('use_fresh_backup_directory')
            saved.write_bytes(old); saved.chmod(0o600)
        path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        fd, temp = tempfile.mkstemp(prefix='.tincan-install-', dir=path.parent)
        try:
            os.fchmod(fd, mode)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(payload); stream.flush(); os.fsync(stream.fileno())
            os.replace(temp, path)
        finally:
            if os.path.exists(temp): os.unlink(temp)
        receipts.append({'path': str(path), 'sha256': hashlib.sha256(payload).hexdigest(), 'mode': oct(mode), 'previous_existed': old is not None})
    return receipts

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--home', type=Path, default=Path.home())
    p.add_argument('--python', type=Path, required=True)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--preservation-report', type=Path)
    p.add_argument('--project-roots', type=Path)
    p.add_argument('--backup', type=Path)
    p.add_argument('--apply', action='store_true')
    args = p.parse_args()
    preservation = json.loads(args.preservation_report.read_text()) if args.preservation_report else None
    writes, key = plan(args.home, args.python, args.binary, preservation, args.project_roots)
    if args.apply:
        if not args.backup: raise ValueError('backup_directory_required')
        spec = importlib.util.spec_from_file_location('router', HERE / 'router.py')
        router = importlib.util.module_from_spec(spec); spec.loader.exec_module(router)
        key.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        router.key_bytes(key, create=True)
        registry = args.home.resolve() / '.config/tincan/host-session-routes.json'
        if registry.exists():
            with router.private._open_registry_directory(registry) as (fd, name): router.load(fd, name)
        receipts = apply(writes, args.backup)
        print(json.dumps({'installed': receipts, 'hosts_restarted': False, 'model_sessions_created': 0, 'relay_calls': 0, 'codex_hook_trust': 'native review required', 'muse_remote_installed': False}, indent=2))
    else:
        print(json.dumps({'planned_paths': [str(path) for path in writes], 'hosts_restarted': False, 'relay_calls': 0}, indent=2))

if __name__ == '__main__':
    main()
