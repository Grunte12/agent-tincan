import concurrent.futures
import importlib.util
import json
import tempfile
import threading
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('project_create', Path(__file__).with_name('project-create.py'))
c = importlib.util.module_from_spec(spec)
spec.loader.exec_module(c)


class ProjectCreation(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.home = Path(self.temp.name).resolve()
        self.repo = self.home / 'Workspace/repos/one'
        self.repo.mkdir(parents=True)
        self.registry = self.home / 'routes.json'
        self.registry.write_text(json.dumps({'version': 1, 'owners': [], 'sessions': [], 'preserved': []}))
        self.registry.chmod(0o600)
        self.patches = [patch.object(c, 'HOME', self.home), patch.object(c, 'STATE', self.home / 'state'),
                        patch.object(c, 'REGISTRY', self.registry), patch.object(c, 'EXTRA_REGISTRY', self.home / 'extra.json'),
                        patch.object(c, 'preflight')]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in reversed(self.patches):
            p.stop()
        self.temp.cleanup()

    def request(self, host='cursor', project=None):
        return {'agent': host, 'host': host, 'project': str(project or self.repo)}

    def test_each_host_creates_once_then_reuses(self):
        for host in sorted(c.HOSTS):
            with self.subTest(host=host), patch.object(c, 'create_native', return_value='native-' + host) as create:
                first = c.ensure(self.request(host))
                again = c.ensure(self.request(host))
                self.assertEqual(first, again)
                self.assertEqual(first['session_id'], 'native-' + host)
                self.assertEqual(create.call_count, 1)

    def test_existing_owner_is_never_replaced(self):
        request = self.request()
        row = {**request, 'session_id': 'old-owner'}
        self.registry.write_text(json.dumps({'version': 1, 'owners': [row], 'sessions': [row], 'preserved': []}))
        with patch.object(c, 'create_native') as create:
            self.assertEqual(c.ensure(request), row)
            create.assert_not_called()

    def test_two_projects_in_parallel_stay_isolated_and_deduplicated(self):
        other = self.repo.with_name('two')
        other.mkdir()
        calls = []
        lock = threading.Lock()

        def native(host, project, remember):
            with lock:
                calls.append((host, project))
            return 'native-' + Path(project).name

        with patch.object(c, 'create_native', side_effect=native):
            with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
                results = list(pool.map(lambda i: c.ensure(self.request(project=self.repo if i % 2 else other)), range(12)))
        self.assertEqual(len(calls), 2)
        self.assertEqual({(r['project'], r['session_id']) for r in results},
                         {(str(self.repo), 'native-one'), (str(other), 'native-two')})

    def test_uncertain_creation_never_runs_again(self):
        with patch.object(c, 'create_native', side_effect=TimeoutError) as create:
            with self.assertRaises(TimeoutError):
                c.ensure(self.request())
            with self.assertRaisesRegex(RuntimeError, 'uncertain'):
                c.ensure(self.request())
            self.assertEqual(create.call_count, 1)

    def test_ambiguous_discoveries_do_not_create_or_choose_first(self):
        row = self.request()
        self.registry.write_text(json.dumps({'version': 1, 'owners': [], 'preserved': [],
                                            'sessions': [{**row, 'session_id': 'one'}, {**row, 'session_id': 'two'}]}))
        with patch.object(c, 'create_native') as create:
            with self.assertRaisesRegex(RuntimeError, 'ambiguous'):
                c.ensure(row)
            create.assert_not_called()

    def test_failed_preflight_is_retryable_without_a_native_attempt(self):
        with patch.object(c, 'preflight', side_effect=RuntimeError('offline')), patch.object(c, 'create_native') as create:
            with self.assertRaisesRegex(RuntimeError, 'offline'):
                c.ensure(self.request())
            create.assert_not_called()
        with patch.object(c, 'create_native', return_value='native-online') as create:
            self.assertEqual(c.ensure(self.request())['session_id'], 'native-online')
            create.assert_called_once()

    def test_failure_after_receiving_id_keeps_id_and_does_not_create_again(self):
        def fail(host, project, remember):
            remember('native-created')
            raise RuntimeError('verification_not_complete')
        with patch.object(c, 'create_native', side_effect=fail) as create:
            with self.assertRaisesRegex(RuntimeError, 'verification_not_complete'):
                c.ensure(self.request())
            receipt = next(c.STATE.glob('*.json'))
            self.assertEqual(json.loads(receipt.read_text())['binding']['session_id'], 'native-created')
            with self.assertRaisesRegex(RuntimeError, 'uncertain'):
                c.ensure(self.request())
            self.assertEqual(create.call_count, 1)

    def test_host_identity_and_scope_fail_before_native_creation(self):
        for row in [{**self.request(), 'agent': 'another-agent'}, self.request('unknown'),
                    self.request(project=self.home)]:
            with patch.object(c, 'create_native') as create:
                with self.assertRaises(RuntimeError):
                    c.ensure(row)
                create.assert_not_called()

    def test_invalid_native_id_is_not_registered(self):
        with patch.object(c, 'create_native', return_value='../../wrong'), patch.object(c, 'register_local') as register:
            with self.assertRaisesRegex(RuntimeError, 'unconfirmed'):
                c.ensure(self.request())
            register.assert_not_called()


if __name__ == '__main__':
    unittest.main()
