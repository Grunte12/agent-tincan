"""Hermes native pre-tool hook: scope only Tincan calls to the actual conversation."""
import importlib.util
from pathlib import Path

source = Path.home() / '.local/share/tincan/adapters/session-routing/native-context.py'
if not source.exists():
    source = Path(__file__).with_name('native-context.py')
spec = importlib.util.spec_from_file_location('tincan_context', source)
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


def before(tool_name, args, session_id='', **kwargs):
    if not tool_name.startswith(('mcp__agent_tincan__', 'mcp__agent-tincan__', 'agent-tincan_')):
        return None
    from gateway.session_context import get_session_env
    from agent.runtime_cwd import scoped_session_cwd
    sid = get_session_env('HERMES_SESSION_KEY', '') or session_id
    project = scoped_session_cwd()
    if not sid or not project:
        return {'action': 'block', 'message': 'Tincan native session/project identity unavailable; no request sent.'}
    try:
        return {'action': 'modify', 'args': {'host_context': adapter.context('hermes', sid, project)}}
    except Exception:
        return {'action': 'block', 'message': 'Tincan native session registration unavailable; no request sent.'}


def register(ctx):
    ctx.register_hook('pre_tool_call', before)
