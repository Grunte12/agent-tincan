import importlib.util
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(file))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


w = module('native_wake', 'native-session-wake.py')
h = module('hermes_hook', 'hermes-plugin.py')


class NativeWake(unittest.TestCase):
    def setUp(self):
        self.lock = patch.object(w.support, 'lock_session', return_value=99)
        self.close = patch.object(w.os, 'close')
        self.lock.start()
        self.close.start()

    def tearDown(self):
        self.close.stop()
        self.lock.stop()

    def test_opencode_busy_session_is_not_given_another_writer(self):
        with patch.object(w.native, 'opencode', side_effect=[{'id': 'ses_x', 'directory': '/project'},
                          {'ses_x': {'type': 'busy'}}]) as api:
            self.assertEqual(w.wake('opencode', 'ses_x', '/project'), 'already_active')
            self.assertEqual(api.call_count, 2)

    def test_opencode_mismatched_project_is_not_woken(self):
        with patch.object(w.native, 'opencode', return_value={'id': 'ses_x', 'directory': '/wrong'}) as api:
            with self.assertRaisesRegex(ValueError, 'scope_mismatch'):
                w.wake('opencode', 'ses_x', '/project')
            api.assert_called_once()

    def test_opencode_wakes_exact_session_with_only_fixed_instruction(self):
        with patch.object(w.native, 'opencode', side_effect=[{'id': 'ses_x', 'directory': '/project'}, {}, None]) as api:
            self.assertEqual(w.wake('opencode', 'ses_x', '/project'), 'accepted')
        call = api.call_args
        self.assertEqual(call.args[:3], ('POST', '/project', 'ses_x'))
        self.assertEqual(call.kwargs, {'action': 'prompt_async'})
        self.assertEqual(call.args[3], {'parts': [{'type': 'text', 'text': w.PROMPT}]})

    def test_hermes_uses_resume_and_normal_approval_mode(self):
        with patch.object(w.subprocess, 'run', return_value=SimpleNamespace(returncode=0)) as run:
            self.assertEqual(w.wake('hermes', 'stored-native', '/project'), 'completed')
        command = run.call_args.args[0]
        self.assertIn('--resume', command)
        self.assertIn('stored-native', command)
        self.assertNotIn('-z', command)
        self.assertNotIn('--yolo', command)
        self.assertEqual(run.call_args.kwargs['cwd'], '/project')
        self.assertEqual(run.call_args.kwargs['env']['TINCAN_SESSION_ID'], 'stored-native')

    def test_empty_notice_does_not_wake_a_model(self):
        with patch.object(sys, 'argv', ['wake', '--host', 'hermes']), patch.object(w, 'routes', return_value={'known': '/p'}), \
                patch.dict(os.environ, {'TINCAN_SESSION_NOTIFICATIONS': '[]'}), patch.object(w, 'wake') as wake:
            w.main()
            wake.assert_not_called()

    def test_hermes_hook_replaces_model_identity_with_native_identity(self):
        context = SimpleNamespace(get_session_env=lambda key, default: 'durable-native')
        cwd = SimpleNamespace(scoped_session_cwd=lambda: '/native/project')
        with patch.dict(sys.modules, {'gateway': SimpleNamespace(), 'gateway.session_context': context,
                                     'agent': SimpleNamespace(), 'agent.runtime_cwd': cwd}), \
                patch.object(h.adapter, 'context', return_value='local-signed') as sign:
            result = h.before('mcp__agent_tincan__ask', {'host_context': 'model-guessed'}, session_id='runtime-id')
        sign.assert_called_once_with('hermes', 'durable-native', '/native/project')
        self.assertEqual(result, {'action': 'modify', 'args': {'host_context': 'local-signed'}})

    def test_hermes_hook_does_not_affect_other_tools(self):
        self.assertIsNone(h.before('terminal', {}))


if __name__ == '__main__':
    unittest.main()
