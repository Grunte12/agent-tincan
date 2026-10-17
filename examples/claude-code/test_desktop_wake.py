import contextlib
import concurrent.futures
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from unittest.mock import Mock

spec = importlib.util.spec_from_file_location('desktop_wake', Path(__file__).with_name('desktop-wake.py'))
wake = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wake)
SID = '00000000-0000-4000-8000-000000000051'
OTHER = '00000000-0000-4000-8000-000000000052'

class DesktopWakeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o700)
        self.project = self.root / 'project'
        self.project.mkdir()
        self.registry = self.root / 'routes.json'
        self.registry.write_text(json.dumps({'version': 1, 'routes': [{'project': str(self.project), 'task': 'project', 'session_id': SID}]}))
        self.registry.chmod(0o600)
        self.state = self.root / 'state'
        self.patches = [patch.object(wake, 'REGISTRY', self.registry), patch.object(wake, 'STATE', self.state),
                        patch.object(wake, 'TINCAN', self.root / 'tincan-missing')]
        for p in self.patches:
            p.start()
            self.addCleanup(p.stop)
        self.item = {'session_id': SID, 'cwd': str(self.project), 'transcript_path': '/transcripts/' + SID + '.jsonl',
                     'hook_event_name': 'SessionStart'}

    def run_hook(self, mode, item=None):
        out, err = io.StringIO(), io.StringIO()
        with patch('sys.stdin', io.StringIO(json.dumps(item or self.item))), contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            result = wake.main(mode)
        return result, out.getvalue(), err.getvalue()

    def test_bound_session_registers_only_its_signal(self):
        code, out, _ = self.run_hook('prepare')
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out)['hookSpecificOutput']['watchPaths'], [str(self.state / (SID + '.signal.json'))])

    def test_session_start_preflights_catalog_and_retires_deleted_chat(self):
        tincan = self.root / 'tincan-missing'
        tincan.write_text('binary')
        transcript = self.root / 'transcripts'; transcript.mkdir()
        self.state.mkdir(mode=0o700)
        deleted = OTHER
        wake.discovery.register(dict(self.item, session_id=deleted,
                                      transcript_path=str(transcript / (deleted + '.jsonl'))),
                                self.registry, self.state)
        marker = transcript / (deleted + '.desktop-released.json')
        marker.write_text(json.dumps({'v': 1, 'releasedAt': '2026-10-17T00:00:00Z', 'reason': 'delete'}))
        marker.chmod(0o600)
        with patch.object(wake.subprocess, 'run', return_value=Mock(returncode=0)) as run:
            self.run_hook('prepare')
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(commands[0], [str(tincan), 'session-retire', '--host', 'claude-code', '--session-id', deleted])
        self.assertEqual(commands[1], [str(tincan), 'session-discover', '--host', 'claude-code', '--project', str(self.project)])
        self.assertEqual(run.call_args_list[1].kwargs['env']['TINCAN_SESSION_ID'], SID)

    def test_wrong_transcript_and_subagent_rejected(self):
        for item in [dict(self.item, transcript_path='/transcripts/' + OTHER + '.jsonl'), dict(self.item, agent_id='child')]:
            with self.assertRaises((ValueError, wake.discovery.route.RouteError)):
                self.run_hook('prepare', item)

    def test_wake_deduplicates_and_rearms_for_next_notice(self):
        self.run_hook('prepare')
        path = self.state / (SID + '.signal.json')
        item = dict(self.item, hook_event_name='FileChanged', file_path=str(path), event='change')
        self.assertEqual(self.run_hook('wake', item)[0], 0)
        for sequence in [1, 2]:
            wake.write_json(path, {'session_id': SID, 'count': 1, 'sequence': sequence})
            self.assertEqual(self.run_hook('wake', dict(item, file_path='/other/session.signal.json'))[0], 0)
            code, _, err = self.run_hook('wake', item)
            self.assertEqual(code, 2)
            self.assertIn('no work has been claimed', err)
            self.assertEqual(self.run_hook('wake', item)[0], 0)

    def test_wrong_notice_owner_cannot_wake(self):
        self.run_hook('prepare')
        path = self.state / (SID + '.signal.json')
        wake.write_json(path, {'session_id': OTHER, 'count': 1, 'sequence': 1})
        item = dict(self.item, hook_event_name='FileChanged', file_path=str(path), event='change')
        self.assertEqual(self.run_hook('wake', item)[0], 0)

    def test_wake_audit_distinguishes_filtering_without_logging_input_paths(self):
        self.run_hook('prepare')
        item = dict(self.item, hook_event_name='FileChanged', file_path='/peer-secret-path', event='change')
        self.assertEqual(self.run_hook('wake', item)[0], 0)
        text = (self.state / (SID + '.events.jsonl')).read_text()
        events = [json.loads(line) for line in text.splitlines()]
        self.assertEqual(events[-2]['event'], 'wake_hook_invoked')
        self.assertFalse(events[-2]['file_matches'])
        self.assertEqual(events[-1]['reason'], 'wrong_path_or_unlink')
        self.assertNotIn('/peer-secret-path', text)

    def test_native_listener_environment_uses_trusted_host_id(self):
        with patch.dict(os.environ, {'CODEX_THREAD_ID': OTHER, 'CLAUDE_CODE_SESSION_ID': OTHER, 'TINCAN_SESSION_ID': OTHER}):
            env = wake.environment(SID)
        self.assertEqual(env['TINCAN_SESSION_ID'], SID)
        self.assertNotIn('CODEX_THREAD_ID', env)
        self.assertNotIn('CLAUDE_CODE_SESSION_ID', env)

    def test_symlink_state_and_public_registry_rejected(self):
        self.state.symlink_to(self.project, target_is_directory=True)
        with self.assertRaises(ValueError):
            self.run_hook('prepare')
        self.registry.chmod(0o644)
        with self.assertRaises((ValueError, wake.discovery.route.RouteError)):
            wake.route_for(SID)

    def test_new_chat_discovered_and_wakes_without_changing_project_owner(self):
        before = self.registry.read_bytes()
        item = dict(self.item, session_id=OTHER, transcript_path='/transcripts/' + OTHER + '.jsonl')
        self.run_hook('prepare', item)
        self.assertEqual(self.registry.read_bytes(), before)
        self.assertEqual(wake.route_for(OTHER), self.project)
        path = self.state / (OTHER + '.signal.json')
        wake.write_json(path, {'session_id': OTHER, 'count': 1, 'sequence': 1})
        self.assertEqual(self.run_hook('wake', dict(item, hook_event_name='FileChanged', file_path=str(path)))[0], 2)
        self.assertEqual(self.run_hook('wake', dict(self.item, hook_event_name='FileChanged', file_path=str(path)))[0], 0)

    def test_resume_keeps_one_registration_and_original_project_in_subdirectory(self):
        self.run_hook('prepare')
        child = self.project / 'src'; child.mkdir()
        self.run_hook('prepare', dict(self.item, cwd=str(child), hook_event_name='CwdChanged'))
        self.run_hook('prepare')
        self.assertEqual(len(list((self.state / 'sessions').glob('*.session.json'))), 1)
        self.assertEqual(wake.route_for(SID, child), self.project)

    def test_discovery_never_reads_transcript_or_starts_claude(self):
        with patch.object(wake.subprocess, 'Popen') as launch:
            self.run_hook('prepare')
        launch.assert_not_called()
        self.assertFalse(Path(self.item['transcript_path']).exists())

    def test_invalid_or_cross_project_events_cannot_register_session(self):
        for item in [dict(self.item, session_id='not-uuid'),
                     dict(self.item, transcript_path=SID + '.jsonl'),
                     dict(self.item, hook_event_name='FileChanged')]:
            with self.assertRaises((ValueError, wake.discovery.route.RouteError)):
                self.run_hook('prepare', item)
        self.run_hook('prepare')
        other_project = self.root / 'other'; other_project.mkdir()
        before = self.registry.read_bytes()
        self.run_hook('prepare', dict(self.item, cwd=str(other_project), hook_event_name='CwdChanged'))
        self.assertEqual(wake.route_for(SID), self.project)
        self.assertEqual(self.registry.read_bytes(), before)
        path = self.state / (SID + '.signal.json')
        wake.write_json(path, {'session_id': SID, 'count': 1, 'sequence': 1})
        code, _, err = self.run_hook('wake', dict(self.item, cwd=str(other_project), hook_event_name='FileChanged', file_path=str(path)))
        self.assertEqual(code, 2)
        self.assertIn(str(self.project), err)
        self.assertIn('Verify each request', err)

    def test_concurrent_registration_atomic_and_discovery_directory_private(self):
        wake.state_directory()
        def register(sid):
            return wake.discovery.register(dict(self.item, session_id=sid, transcript_path='/transcripts/' + sid + '.jsonl'), self.registry, self.state)
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            self.assertEqual(list(pool.map(register, [SID, OTHER, SID, OTHER])), [self.project] * 4)
        records = wake.discovery.candidates(self.project, self.state)
        self.assertEqual({r['session_id'] for r in records}, {SID, OTHER})
        record = self.state / 'sessions' / (OTHER + '.session.json')
        record.chmod(0o644)
        with self.assertRaises(wake.discovery.route.RouteError):
            wake.discovery.project_for(OTHER, self.registry, self.state)

    def test_duplicate_listener_does_not_start_another_process(self):
        import fcntl
        self.run_hook('prepare')
        with open(self.state / (SID + '.listen.lock'), 'w') as lock:
            os.chmod(lock.name, 0o600)
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with patch.object(wake, 'run_listener') as run:
                self.assertEqual(self.run_hook('listen')[0], 0)
            run.assert_not_called()

    def test_listener_crash_restarts_but_host_exit_stops(self):
        self.run_hook('prepare')
        children = [Mock(pid=111), Mock(pid=222)]
        for child in children:
            child.poll.return_value = 1
            child.wait.return_value = 1
        host = {'pid': 123}
        def sleep(_seconds):
            if children[1].wait.called:
                host['pid'] = 1
        with patch.object(wake.os, 'getppid', side_effect=lambda: host['pid']), \
                patch.object(wake.subprocess, 'Popen', side_effect=children) as launch, \
                patch.object(wake.signal, 'signal'), patch.object(wake.time, 'sleep', side_effect=sleep):
            wake.run_listener(SID, self.project)
        self.assertEqual(launch.call_count, 2)
        for call in launch.call_args_list:
            self.assertEqual(call.kwargs['env']['TINCAN_SESSION_ID'], SID)

    def test_listener_does_not_leave_child_when_host_disappears(self):
        self.run_hook('prepare')
        child = Mock(pid=111)
        child.poll.side_effect = [None, None, 0]
        child.wait.return_value = 0
        with patch.object(wake.os, 'getppid', side_effect=[123, 123, 1, 1]), \
                patch.object(wake.subprocess, 'Popen', return_value=child), patch.object(wake.signal, 'signal'):
            wake.run_listener(SID, self.project)
        child.terminate.assert_called_once()

    def test_orphan_migration_checks_exact_scoped_native_child(self):
        self.run_hook('prepare')
        wake.record(SID, 'listener_started', pid=111, tincan_pid=222, host_parent_pid=333)
        script = str(Path(wake.__file__).resolve())
        orphan = (1, '/python ' + script + ' listen')
        child = (111, str(wake.TINCAN) + ' listen --exec /python ' + script + ' notice ' + SID)
        with patch.object(wake, 'process_identity', side_effect=[orphan, child, orphan, child]), patch.object(wake.os, 'kill') as kill:
            self.assertTrue(wake.reclaim_orphan(SID))
        kill.assert_called_once_with(111, wake.signal.SIGTERM)

    def test_live_listener_and_wrong_scope_are_never_reclaimed(self):
        self.run_hook('prepare')
        wake.record(SID, 'listener_started', pid=111, tincan_pid=222, host_parent_pid=333)
        script = str(Path(wake.__file__).resolve())
        for owner, child in [((333, '/python ' + script + ' listen'), (111, str(wake.TINCAN) + ' listen --exec /python ' + script + ' notice ' + SID)),
                             ((1, '/python ' + script + ' listen'), (111, str(wake.TINCAN) + ' listen --exec /python ' + script + ' notice ' + OTHER)),
                             ((1, '/some-other-task listen'), (111, str(wake.TINCAN) + ' listen --exec /python ' + script + ' notice ' + SID))]:
            with patch.object(wake, 'process_identity', side_effect=[owner, child]), patch.object(wake.os, 'kill') as kill:
                self.assertFalse(wake.reclaim_orphan(SID))
            kill.assert_not_called()

if __name__ == '__main__':
    unittest.main()
