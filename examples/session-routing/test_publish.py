from __future__ import annotations

import json
import io
import os
from pathlib import Path
import tempfile
import types
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("router.py")
router = types.ModuleType("router")
router.__file__ = str(SCRIPT)
exec(compile(SCRIPT.read_text(encoding="utf-8"), str(SCRIPT), "exec"), router.__dict__)


class Completed:
    def __init__(self, stdout, returncode=0):
        self.stdout = stdout
        self.returncode = returncode
        self.stderr = ""


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=str(Path(tempfile.gettempdir()).resolve()))
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.private = self.root / "private"
        self.private.mkdir(mode=0o700)
        self.private.chmod(0o700)
        self.local = self.root / "work"
        self.nested = self.local / "repo"
        self.nested.mkdir(parents=True)
        self.common = Path("/shared/work")
        self.nested_common = Path("/shared/monorepo")
        self.roots = self.root / "project-roots.json"
        self.roots.write_text(json.dumps([
            {"local": str(self.local), "communication": str(self.common)},
            {"local": str(self.nested), "communication": str(self.nested_common)},
        ]), encoding="utf-8")
        self.roots.chmod(0o600)
        self.tincan = self.root / "tincan"
        self.config = self.root / "tincan.json"

    def run_hook_main(self, event, extra_args, publish_result):
        output, errors = io.StringIO(), io.StringIO()
        argv = ["router.py", "hook", "--host", "cursor", "--agent", "agent",
                "--registry", str(self.root / "private/routes.json"), "--key", str(self.root / "private/key"),
                *extra_args]
        class Stdin:
            buffer = io.BytesIO(json.dumps(event).encode())
        with patch.object(router.sys, "argv", argv), patch.object(router.sys, "stdin", Stdin()), \
                patch.object(router.subprocess, "run", return_value=publish_result) as run, \
                redirect_stdout(output), redirect_stderr(errors):
            code = router.main()
        return code, output.getvalue(), errors.getvalue(), run

    def tearDown(self):
        self.temp.cleanup()

    def test_communication_mapping_uses_most_specific_root(self):
        self.assertEqual(
            router.communication_project(str(self.nested / "src"), self.roots),
            "/shared/monorepo/src",
        )
        self.assertEqual(
            router.communication_project(str(self.local / "docs"), self.roots),
            "/shared/work/docs",
        )

    def test_communication_mapping_rejects_invalid_and_duplicate_roots(self):
        for rows, error in (
            ([{"local": "relative", "communication": "/shared"}], "invalid_project_roots"),
            ([{"local": str(self.local), "communication": "/shared"}, {"local": str(self.local), "communication": "/other"}], "duplicate_project_root"),
        ):
            with self.subTest(error=error):
                self.roots.write_text(json.dumps(rows), encoding="utf-8")
                self.roots.chmod(0o600)
                with self.assertRaisesRegex(router.Error, error):
                    router.communication_project(str(self.local), self.roots)

    def test_publish_uses_actual_native_session_and_private_direct_command(self):
        row = {"agent": "muse", "host": "muse", "project": str(self.nested), "session_id": "actual-session"}
        response = {"agent": "muse", "host": "muse", "project": "/shared/monorepo", "session_id": "actual-session"}
        seen = {}
        inherited_parent = os.environ.get("TINCAN_SESSION_ID")

        def run(args, **kwargs):
            seen["args"] = args
            seen["env"] = kwargs["env"].copy()
            return Completed(json.dumps(response))

        with patch.dict(os.environ, {"TINCAN_SESSION_ID": "inherited-session", "CODEX_THREAD_ID": "codex-thread", "CLAUDE_CODE_SESSION_ID": "claude-session"}), patch.object(router.subprocess, "run", side_effect=run):
            binding = router.publish(row, self.tincan, self.config, self.roots)

        self.assertEqual(binding, response)
        self.assertEqual(seen["args"], [str(self.tincan), "session-discover", "--host", "muse", "--project", "/shared/monorepo"])
        self.assertEqual(seen["env"]["TINCAN_SESSION_ID"], "actual-session")
        self.assertEqual(seen["env"]["TINCAN_CONFIG"], str(self.config))
        self.assertNotIn("CODEX_THREAD_ID", seen["env"])
        self.assertNotIn("CLAUDE_CODE_SESSION_ID", seen["env"])
        self.assertEqual(os.environ.get("TINCAN_SESSION_ID"), inherited_parent)

    def test_publish_rejects_response_identity_mismatch(self):
        row = {"agent": "muse", "host": "muse", "project": str(self.nested), "session_id": "actual-session"}
        wrong = {"agent": "muse", "host": "muse", "project": "/shared/monorepo", "session_id": "other-session"}
        with patch.object(router.subprocess, "run", return_value=Completed(json.dumps(wrong))):
            with self.assertRaisesRegex(router.Error, "relay_discovery_identity_mismatch"):
                router.publish(row, self.tincan, self.config, self.roots)

    def test_publish_requires_direct_discovery_success(self):
        row = {"agent": "muse", "host": "muse", "project": str(self.nested), "session_id": "actual-session"}
        with patch.object(router.subprocess, "run", return_value=Completed("", returncode=1)):
            with self.assertRaisesRegex(router.Error, "relay_session_discovery_unavailable"):
                router.publish(row, self.tincan, self.config, self.roots)

    def test_hook_publish_keeps_known_project_for_changed_multiroot_workspace(self):
        registry = self.private / "routes.json"
        key = self.private / "key"
        router.key_bytes(key, create=True)
        original = router.update(registry, "agent", "cursor", str(self.nested), "stable-conversation")
        response = {"agent": "agent", "host": "cursor", "project": "/shared/monorepo", "session_id": "stable-conversation"}
        event = {"hook_event_name": "beforeSubmitPrompt", "conversation_id": "stable-conversation",
                 "workspace_roots": [str(self.local), str(self.nested)]}
        code, stdout, stderr, run = self.run_hook_main(event, [
            "--publish", "--tincan-bin", str(self.tincan), "--config", str(self.config), "--project-roots", str(self.roots),
        ], Completed(json.dumps(response)))
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout), {"continue": True})
        self.assertEqual(stderr, "")
        self.assertEqual(original["project"], str(self.nested))
        self.assertEqual(run.call_args.args[0], [str(self.tincan), "session-discover", "--host", "cursor", "--project", "/shared/monorepo"])
        self.assertEqual(run.call_args.kwargs["env"]["TINCAN_SESSION_ID"], "stable-conversation")

    def test_hook_publish_failure_keeps_native_hook_stdout_and_makes_no_queue_call(self):
        registry = self.private / "routes.json"
        key = self.private / "key"
        router.key_bytes(key, create=True)
        router.update(registry, "agent", "cursor", str(self.nested), "stable-conversation")
        event = {"hook_event_name": "beforeSubmitPrompt", "conversation_id": "stable-conversation",
                 "workspace_roots": [str(self.local), str(self.nested)]}
        code, stdout, stderr, run = self.run_hook_main(event, [
            "--publish", "--tincan-bin", str(self.tincan), "--config", str(self.config), "--project-roots", str(self.roots),
        ], Completed("", returncode=1))
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout), {"continue": True})
        self.assertEqual(stderr.strip(), "relay_session_discovery_unavailable")
        self.assertEqual(run.call_args.args[0][1], "session-discover")

    def test_cursor_cli_mcp_gate_publishes_native_id_without_startup_but_skips_other_servers(self):
        key = self.private / 'key'
        router.key_bytes(key, create=True)
        event = {'hook_event_name': 'beforeMCPExecution', 'conversation_id': 'cli-conversation',
                 'workspace_roots': [str(self.nested)], 'mcp_server_name': 'agent-tincan',
                 'tool_name': 'claim', 'tool_input': json.dumps({'request_id': 'probe'})}
        response = {'agent': 'agent', 'host': 'cursor', 'project': '/shared/monorepo', 'session_id': 'cli-conversation'}
        options = ['--publish', '--tincan-bin', str(self.tincan), '--config', str(self.config), '--project-roots', str(self.roots)]
        code, stdout, stderr, run = self.run_hook_main(event, options, Completed(json.dumps(response)))
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout)['permission'], 'deny')
        self.assertEqual(stderr, '')
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.kwargs['env']['TINCAN_SESSION_ID'], 'cli-conversation')
        self.assertEqual(run.call_args.args[0][1], 'session-discover')
        code, stdout, stderr, run = self.run_hook_main({**event, 'mcp_server_name': 'other'}, options, Completed(''))
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout), {'permission': 'allow'})
        self.assertEqual(run.call_count, 0)


if __name__ == "__main__":
    unittest.main()
