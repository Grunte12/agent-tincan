import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('project_session', Path(__file__).with_name('project-session.py'))
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)

class ProjectSessions(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name).resolve(); self.root.chmod(0o700)
        self.project = self.root / 'project'; self.project.mkdir()
        self.registry = self.root / 'routes.json'
        self.env = patch.dict(os.environ, {'TINCAN_PROJECT_ROOTS': json.dumps([str(self.root)])})
        self.env.start()
        self.bootstraps = []

    def discover(self, sid):
        state = self.root / 'desktop-state'; state.mkdir(mode=0o700, exist_ok=True)
        adapter.discovery.register({'session_id': sid, 'cwd': str(self.project),
                                    'transcript_path': '/transcripts/' + sid + '.jsonl'}, self.registry, state)
        return state

    def test_unknown_project_does_not_create_by_default(self):
        with patch.object(adapter.subprocess, 'run') as run:
            with self.assertRaisesRegex(adapter.route.RouteError, 'not_discovered'):
                adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', state=self.root / 'absent')
        run.assert_not_called(); self.assertFalse(self.registry.exists())
        self.assertFalse(list(self.root.glob('*.creation-*.json')))

    def test_unique_discovered_session_selected_once_under_concurrent_sends(self):
        original = '00000000-0000-4000-8000-000000000001'
        state = self.discover(original)
        with patch.object(adapter.subprocess, 'run') as run:
            with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                results = list(pool.map(lambda _: adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', state=state), range(4)))
        run.assert_not_called()
        self.assertEqual({r['session_id'] for r in results}, {original})
        self.assertEqual(sum(r['action'] == 'discovered' for r in results), 1)
        self.assertEqual(len(json.loads(self.registry.read_text())['routes']), 1)

    def test_second_discovered_chat_cannot_steal_selected_owner(self):
        original = '00000000-0000-4000-8000-000000000001'
        state = self.discover(original)
        adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', state=state)
        before = self.registry.read_bytes()
        self.discover('00000000-0000-4000-8000-000000000002')
        with patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', state=state)
        run.assert_not_called()
        self.assertEqual(result['session_id'], original)
        self.assertEqual(self.registry.read_bytes(), before)

    def test_deleted_desktop_owner_is_pruned_and_live_session_takes_over(self):
        deleted = '00000000-0000-4000-8000-000000000001'
        active = '00000000-0000-4000-8000-000000000002'
        transcript_dir = self.root / 'transcripts'; transcript_dir.mkdir()
        state = self.root / 'desktop-state'; state.mkdir(mode=0o700)
        for sid in (deleted, active):
            adapter.discovery.register({
                'session_id': sid, 'cwd': str(self.project),
                'transcript_path': str(transcript_dir / (sid + '.jsonl')),
            }, self.registry, state)
        marker = transcript_dir / (deleted + '.desktop-released.json')
        marker.write_text(json.dumps({'v': 1, 'releasedAt': '2026-10-17T00:00:00Z', 'reason': 'delete'}))
        marker.chmod(0o600)
        adapter.route.bind_route(self.registry, self.project, 'project', deleted)

        with patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(
                self.registry, self.project, 'claude-sonnet-5-5', state=state)

        run.assert_not_called()
        self.assertEqual(result['session_id'], active)
        routes = json.loads(self.registry.read_text())['routes']
        self.assertEqual(routes, [{'project': str(self.project), 'task': 'project', 'session_id': active}])

    def test_multiple_discovered_chats_without_owner_require_decision(self):
        for sid in ['00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002']:
            state = self.discover(sid)
        with patch.object(adapter.subprocess, 'run') as run:
            with self.assertRaisesRegex(adapter.route.RouteError, 'multiple_project_sessions'):
                adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', state=state)
        run.assert_not_called(); self.assertFalse(self.registry.exists())

    def test_explicit_default_owner_wins_among_task_specific_routes(self):
        original = '00000000-0000-4000-8000-000000000001'
        adapter.route.bind_route(self.registry, self.project, 'project', original)
        adapter.route.bind_route(self.registry, self.project, 'design', '00000000-0000-4000-8000-000000000002')
        with patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5')
        run.assert_not_called(); self.assertEqual(result['session_id'], original)

    def test_worktree_project_is_not_collapsed_into_main_checkout(self):
        state = self.discover('00000000-0000-4000-8000-000000000001')
        worktree = self.root / 'worktree'; worktree.mkdir()
        with patch.object(adapter.subprocess, 'run') as run:
            with self.assertRaisesRegex(adapter.route.RouteError, 'not_discovered'):
                adapter.resolve_or_create(self.registry, worktree, 'claude-sonnet-5-5', state=state)
        run.assert_not_called()

    def tearDown(self):
        self.env.stop(); self.temp.cleanup()

    def run_cli(self, argv, **kwargs):
        if argv[1:3] == ['auth', 'status']:
            return SimpleNamespace(returncode=0, stdout='{"loggedIn":true}')
        self.bootstraps.append(argv)
        self.assertNotIn('--fallback-model', argv)
        self.assertEqual(argv[argv.index('--tools') + 1], '')
        return SimpleNamespace(returncode=0, stdout=json.dumps({
            'session_id': argv[argv.index('--session-id') + 1],
            'modelUsage': {'claude-sonnet-5-5': {}}, 'is_error': False
        }))

    def test_concurrent_requests_create_one_session_and_then_reuse(self):
        with patch.object(adapter.subprocess, 'run', self.run_cli):
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                results = list(pool.map(lambda _: adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', allow_create=True, state=self.root / 'absent'), range(2)))
        self.assertEqual(len(self.bootstraps), 1)
        self.assertEqual(results[0]['session_id'], results[1]['session_id'])
        self.assertEqual(sorted(r['action'] for r in results), ['created', 'resolved'])
        self.assertEqual(len(json.loads(self.registry.read_text())['routes']), 1)

    def test_existing_project_route_never_creates_another_model(self):
        original = '00000000-0000-4000-8000-000000000001'
        adapter.route.bind_route(self.registry, self.project, 'design', original)
        with patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5')
        run.assert_not_called(); self.assertEqual(result['session_id'], original)
        self.assertFalse(result['model_verified'])

    def test_existing_owner_needs_no_model_and_never_changes_it(self):
        original = '00000000-0000-4000-8000-000000000001'
        adapter.route.bind_route(self.registry, self.project, 'project', original)
        with patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(self.registry, self.project, '')
        run.assert_not_called()
        self.assertEqual(result['session_id'], original)

    def test_new_project_without_model_uses_native_default_once(self):
        with patch.object(adapter.subprocess, 'run', self.run_cli):
            result = adapter.resolve_or_create(self.registry, self.project, '', allow_create=True, state=self.root / 'absent')
        self.assertEqual(result['action'], 'created')
        self.assertEqual(len(self.bootstraps), 1)
        self.assertNotIn('--model', self.bootstraps[0])

    def test_wrong_model_stops_and_never_retries_creation(self):
        def wrong(argv, **kwargs):
            result = self.run_cli(argv, **kwargs)
            if argv[1:3] != ['auth', 'status']:
                body = json.loads(result.stdout); body['modelUsage'] = {'other-model': {}}
                result.stdout = json.dumps(body)
            return result
        with patch.object(adapter.subprocess, 'run', wrong):
            for _ in range(2):
                with self.assertRaisesRegex(adapter.route.RouteError, 'needs_review'):
                    adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', allow_create=True, state=self.root / 'absent')
        self.assertEqual(len(self.bootstraps), 1)
        self.assertFalse(self.registry.exists())

    def test_explicit_existing_binding_outside_bootstrap_roots_is_reused(self):
        approved = self.root / 'bootstrap-only'; approved.mkdir()
        original = '00000000-0000-4000-8000-000000000001'
        adapter.route.bind_route(self.registry, self.project, 'project', original)
        with patch.dict(os.environ, {'TINCAN_PROJECT_ROOTS': json.dumps([str(approved)])}), patch.object(adapter.subprocess, 'run') as run:
            result = adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5')
        run.assert_not_called()
        self.assertEqual(result['session_id'], original)

    def test_unbound_project_outside_bootstrap_roots_cannot_create(self):
        approved = self.root / 'bootstrap-only'; approved.mkdir()
        with patch.dict(os.environ, {'TINCAN_PROJECT_ROOTS': json.dumps([str(approved)])}), patch.object(adapter.subprocess, 'run') as run:
            with self.assertRaisesRegex(adapter.route.RouteError, 'project_outside_operator_roots'):
                adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', allow_create=True, state=self.root / 'absent')
        run.assert_not_called()
        self.assertFalse(self.registry.exists())
        self.assertFalse(list(self.root.glob('*.creation-*.json')))

    def test_multiple_existing_sessions_require_owner_decision(self):
        for task, uuid in [('design', '00000000-0000-4000-8000-000000000001'), ('code', '00000000-0000-4000-8000-000000000002')]:
            adapter.route.bind_route(self.registry, self.project, task, uuid)
        with self.assertRaisesRegex(adapter.route.RouteError, 'multiple_project_sessions'):
            adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5')

    def test_unauthenticated_creation_does_not_reserve_or_launch(self):
        with patch.object(adapter.subprocess, 'run', return_value=SimpleNamespace(returncode=1, stdout='{"loggedIn":false}')) as run:
            with self.assertRaisesRegex(adapter.route.RouteError, 'auth_required'):
                adapter.resolve_or_create(self.registry, self.project, 'claude-sonnet-5-5', allow_create=True, state=self.root / 'absent')
        self.assertEqual(run.call_count, 1); self.assertFalse(self.registry.exists())
        self.assertFalse(list(self.root.glob('*.creation-*.json')))

if __name__ == '__main__': unittest.main()
