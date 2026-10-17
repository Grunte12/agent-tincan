from __future__ import annotations

import json
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("muse-wait.py")
muse_wait = types.ModuleType("muse_wait")
sys.modules[muse_wait.__name__] = muse_wait
muse_wait.__file__ = str(SCRIPT)
exec(compile(SCRIPT.read_text(encoding="utf-8"), str(SCRIPT), "exec"), muse_wait.__dict__)


class MuseWaitTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=str(Path(tempfile.gettempdir()).resolve()))
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.project = self.root / "project"
        self.project.mkdir()
        self.other = self.root / "other"
        self.other.mkdir()
        self.private = self.root / "private"
        self.private.mkdir(mode=0o700)
        self.private.chmod(0o700)
        self.registry = self.private / "routes.json"
        self.roots = self.private / "project-roots.json"
        self.common = Path("/shared/muse-project")
        self.roots.write_text(json.dumps([{"local": str(self.project), "communication": str(self.common)}]), encoding="utf-8")
        self.roots.chmod(0o600)
        self.config = self.private / "muse.json"
        self.tincan = self.root / "tincan"

    def tearDown(self):
        self.temp.cleanup()

    def test_prepare_registers_actual_runtime_session_and_preserves_environment(self):
        environment = {
            "CODEX_THREAD_ID": "inherited-codex-placeholder",
            "CLAUDE_CODE_SESSION_ID": "inherited-claude-placeholder",
            "TINCAN_SESSION_ID": "old-placeholder",
            "HTTP_PROXY": "http://proxy.invalid:8080",
            "TINCAN_CONFIG": "/synthetic/config.json",
            "HATCH_AUTH_TOKEN": "synthetic-auth-value",
        }
        event = {"sessionId": "muse-session-1", "cwd": str(self.project)}
        with patch.object(muse_wait.os, "execvpe") as execvpe, patch.object(subprocess, "Popen") as popen:
            row, updated = muse_wait.prepare(event, "linkhub-muse", self.registry, environment)
        execvpe.assert_not_called()
        popen.assert_not_called()
        self.assertEqual(row["agent"], "linkhub-muse")
        self.assertEqual(row["host"], "muse")
        self.assertEqual(row["project"], str(self.project))
        self.assertEqual(row["session_id"], "muse-session-1")
        self.assertEqual(updated["TINCAN_SESSION_ID"], "muse-session-1")
        self.assertNotIn("CODEX_THREAD_ID", updated)
        self.assertNotIn("CLAUDE_CODE_SESSION_ID", updated)
        for name in ("HTTP_PROXY", "TINCAN_CONFIG", "HATCH_AUTH_TOKEN"):
            self.assertEqual(updated[name], environment[name])
        self.assertEqual(environment["TINCAN_SESSION_ID"], "old-placeholder")
        state = json.loads(self.registry.read_text())
        self.assertEqual(state["sessions"], [row])

    def test_prepare_migrates_only_the_preserved_owner_and_keeps_project(self):
        state = {"version": 1, "owners": [], "sessions": [],
                 "preserved": [{"agent": "linkhub-muse", "host": "muse", "session_id": "old-muse-session"}]}
        self.registry.write_text(json.dumps(state))
        self.registry.chmod(0o600)
        row, _ = muse_wait.prepare({"sessionId": "old-muse-session", "cwd": str(self.project)}, "linkhub-muse", self.registry, {})
        self.assertEqual(row["project"], str(self.project))
        first = json.loads(self.registry.read_text())
        self.assertEqual(first["owners"], [row])
        # Runtime-reported cwd changes do not transfer an existing conversation.
        row2, _ = muse_wait.prepare({"sessionId": "old-muse-session", "cwd": str(self.other)}, "linkhub-muse", self.registry, {})
        self.assertEqual(row2["project"], str(self.project))
        self.assertEqual(json.loads(self.registry.read_text())["owners"], [row])

    def test_invalid_runtime_context_fails_closed_without_exec_or_process_launch(self):
        invalid_events = (
            {"sessionId": "", "cwd": str(self.project)},
            {"sessionId": "bad session id", "cwd": str(self.project)},
            {"sessionId": "muse-session", "cwd": "relative/path"},
            {"sessionId": "muse-session"},
        )
        for event in invalid_events:
            with self.subTest(event=event), patch.object(muse_wait.os, "execvpe") as execvpe, patch.object(subprocess, "Popen") as popen:
                with self.assertRaises(muse_wait.router.Error):
                    muse_wait.prepare(event, "linkhub-muse", self.registry, {})
                execvpe.assert_not_called()
                popen.assert_not_called()
        self.assertFalse(self.registry.exists())

    def run_main(self, event, args, publish_result=None):
        output, errors, calls = io.StringIO(), io.StringIO(), []
        class Stdin:
            buffer = io.BytesIO(json.dumps(event).encode())
        argv = ["muse-wait.py", "--registry", str(self.registry), "--tincan", str(self.tincan), *args]
        def run(command, **kwargs):
            calls.append(("discover", list(command), kwargs["env"].copy()))
            class Completed:
                returncode = 1 if publish_result is None else 0
                stdout = "" if publish_result is None else json.dumps(publish_result)
            return Completed()
        def execvpe(file, argv, env):
            calls.append(("exec", file, list(argv), env.copy()))
        with patch.object(muse_wait.sys, "argv", argv), patch.object(muse_wait.sys, "stdin", Stdin()), \
                patch.object(muse_wait.router.subprocess, "run", side_effect=run), patch.object(muse_wait.os, "execvpe", side_effect=execvpe), \
                patch.object(muse_wait.os, "chdir"), \
                redirect_stdout(output), redirect_stderr(errors):
            code = muse_wait.main()
        return code, output.getvalue(), errors.getvalue(), calls

    def test_publish_registers_runtime_session_before_wait(self):
        event = {"sessionId": "actual-muse-runtime-session", "cwd": str(self.project)}
        binding = {"agent": "linkhub-muse", "host": "muse", "project": str(self.common), "session_id": event["sessionId"]}
        code, output, errors, calls = self.run_main(event, [
            "--publish", "--config", str(self.config), "--project-roots", str(self.roots),
        ], binding)
        self.assertIn(code, (0, None))  # execvpe does not return in production.
        self.assertEqual(errors, "")
        self.assertEqual(calls[0][0:2], ("discover", [str(self.tincan), "session-discover", "--host", "muse", "--project", "/shared/muse-project"]))
        self.assertEqual(calls[0][2]["TINCAN_SESSION_ID"], event["sessionId"])
        self.assertEqual(calls[0][2]["TINCAN_CONFIG"], str(self.config))
        self.assertEqual(calls[1][0:3], ("exec", str(self.tincan), [str(self.tincan), "wait"]))
        self.assertEqual(calls[1][3]["TINCAN_SESSION_ID"], event["sessionId"])
        self.assertEqual(calls[1][3]["TINCAN_CONFIG"], str(self.config))
        self.assertNotIn("CODEX_THREAD_ID", calls[1][3])
        self.assertNotIn("CLAUDE_CODE_SESSION_ID", calls[1][3])
        self.assertEqual(output, "")

    def test_publish_failure_prevents_wait_exec(self):
        event = {"sessionId": "actual-muse-runtime-session", "cwd": str(self.project)}
        code, output, errors, calls = self.run_main(event, ["--publish", "--config", str(self.config), "--project-roots", str(self.roots)])
        self.assertEqual(code, 2)
        self.assertEqual(output, "")
        self.assertEqual(errors.strip(), "relay_session_discovery_unavailable")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][0], "discover")

    def test_register_only_publishes_metadata_without_wait_or_new_session(self):
        event = {"sessionId": "actual-muse-runtime-session", "cwd": str(self.project)}
        binding = {"agent": "linkhub-muse", "host": "muse", "project": str(self.common), "session_id": event["sessionId"]}
        code, output, errors, calls = self.run_main(event, [
            "--publish", "--register-only", "--config", str(self.config), "--project-roots", str(self.roots),
        ], binding)
        self.assertEqual(code, 0)
        self.assertEqual(errors, "")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][0], "discover")
        self.assertEqual(json.loads(output)["session_id"], event["sessionId"])
        self.assertNotIn("exec", [item[0] for item in calls])


if __name__ == "__main__":
    unittest.main()
