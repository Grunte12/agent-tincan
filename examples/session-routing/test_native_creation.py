import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('native_create', Path(__file__).with_name('project-create.py'))
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class NativeCreation(unittest.TestCase):
    project = '/native/project'

    def test_cursor_uses_native_create_chat_in_exact_workspace(self):
        with patch.object(c, 'native_cli', return_value='native-id\n') as run:
            remember = Mock()
            self.assertEqual(c.create_native('cursor', self.project, remember), 'native-id')
            command, cwd = run.call_args.args
            self.assertEqual(command[-3:], ['--workspace', self.project, 'create-chat'])
            self.assertEqual(cwd, self.project)
            remember.assert_called_once_with('native-id')

    def test_codex_checks_persistence_without_starting_a_turn(self):
        rpc = Mock()
        thread = {'id': 'native-codex', 'cwd': self.project, 'ephemeral': False}
        rpc.call.side_effect = [{}, {'thread': thread}, {}, {'thread': thread}]
        remember = Mock()
        with patch.object(c, 'RPC', return_value=rpc):
            self.assertEqual(c.create_native('codex', self.project, remember), 'native-codex')
        self.assertEqual([call.args[0] for call in rpc.call.call_args_list],
                         ['initialize', 'thread/start', 'thread/name/set', 'thread/read'])
        remember.assert_called_once_with('native-codex')
        rpc.close.assert_called_once()

    def test_codex_wrong_project_is_not_bound(self):
        rpc = Mock()
        rpc.call.side_effect = [{}, {'thread': {'id': 'wrong', 'cwd': '/another'}}]
        remember = Mock()
        with patch.object(c, 'RPC', return_value=rpc):
            with self.assertRaisesRegex(RuntimeError, 'identity_mismatch'):
                c.create_native('codex', self.project, remember)
        remember.assert_not_called()

    def test_hermes_uses_published_runtime_and_durable_id(self):
        rpc = Mock()
        rpc.call.side_effect = [{'session_id': 'runtime-id', 'stored_session_id': 'stored-id',
                                 'info': {'cwd': self.project}}, {'pending': False, 'title': 'project'}]
        with patch.object(c, 'RPC', return_value=rpc) as start:
            self.assertEqual(c.create_native('hermes', self.project), 'stored-id')
        self.assertEqual(start.call_args.args[0][-2:], ['--run-module', 'tui_gateway.entry'])
        self.assertEqual(rpc.call.call_args_list[1].args[1]['session_id'], 'runtime-id')

    def test_hermes_unpersisted_draft_is_not_registered(self):
        rpc = Mock()
        rpc.call.side_effect = [{'session_id': 'runtime', 'stored_session_id': 'stored',
                                 'info': {'cwd': self.project}}, {'pending': True}]
        with patch.object(c, 'RPC', return_value=rpc):
            with self.assertRaisesRegex(RuntimeError, 'not_persisted'):
                c.create_native('hermes', self.project)

    def test_opencode_remembers_id_before_verifying_project(self):
        order = []
        def request(method, project, sid='', body=None):
            order.append(method)
            return {'id': 'ses_native', 'directory': self.project}
        with patch.object(c, 'opencode', side_effect=request):
            self.assertEqual(c.create_native('opencode', self.project, lambda sid: order.append(sid)), 'ses_native')
        self.assertEqual(order, ['POST', 'ses_native', 'GET'])

    def test_native_http_does_not_follow_redirects_or_use_proxy(self):
        connection = Mock()
        connection.getresponse.return_value.status = 302
        connection.getresponse.return_value.read.return_value = b''
        with patch.object(c.http.client, 'HTTPConnection', return_value=connection) as http:
            with self.assertRaisesRegex(RuntimeError, 'unavailable'):
                c.opencode('GET', self.project, 'ses_native')
        self.assertEqual(http.call_args.args, ('127.0.0.1', 4096))
        connection.close.assert_called_once()


if __name__ == '__main__':
    unittest.main()
