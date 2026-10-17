import contextlib
import fcntl
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch


SPEC = importlib.util.spec_from_file_location(
    "desktop_wake_receive", Path(__file__).with_name("desktop-wake.py"))
wake = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(wake)

SID = "00000000-0000-4000-8000-000000000081"
OTHER = "00000000-0000-4000-8000-000000000082"


class DesktopReceiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o700)
        self.project = self.root / "project"
        self.project.mkdir()
        self.registry = self.root / "routes.json"
        self.registry.write_text(json.dumps({
            "version": 1,
            "routes": [{"project": str(self.project), "task": "project", "session_id": SID}],
        }), encoding="utf-8")
        self.registry.chmod(0o600)
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.tincan = self.root / "tincan"
        self.patches = [
            patch.object(wake, "REGISTRY", self.registry),
            patch.object(wake, "STATE", self.state),
            patch.object(wake, "TINCAN", self.tincan),
        ]
        for item in self.patches:
            item.start()
            self.addCleanup(item.stop)
        wake.write_json(self.state / (SID + ".signal.json"), {
            "session_id": SID, "count": 3, "sequence": 17,
        })

    def child(self, polls, code=0):
        process = Mock(pid=9876)
        process.poll.side_effect = polls
        process.wait.return_value = code
        return process

    def launch_with_notice(self, process, sequence=18):
        def launch(*_args, **_kwargs):
            wake.write_json(self.state / (SID + ".signal.json"), {
                "session_id": SID, "count": 1, "sequence": sequence,
            })
            return process
        return launch

    def test_native_receive_listener_is_scoped_and_clears_inherited_identity(self):
        process = self.child([None, 0, 0])
        with patch.dict(os.environ, {
            "CODEX_THREAD_ID": OTHER,
            "CODEX_SESSION_ID": OTHER,
            "CLAUDE_CODE_SESSION_ID": OTHER,
            "TINCAN_SESSION_ID": OTHER,
            "TINCAN_WAITING": "999",
            "GEMINI_AUTH_TOKEN": "native-auth",
            "HTTP_PROXY": "http://proxy.invalid",
        }), patch.object(wake.subprocess, "Popen", side_effect=self.launch_with_notice(process)) as launch, \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "sleep"):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(wake.wait_and_rewake(SID, self.project), 2)
        args, kwargs = launch.call_args
        self.assertEqual(args[0][:3], [str(self.tincan), "listen", "--once"])
        self.assertIn("notice " + SID, args[0][4])
        self.assertEqual(Path(kwargs["cwd"]), self.project)
        child_env = kwargs["env"]
        self.assertEqual(child_env["TINCAN_SESSION_ID"], SID)
        self.assertEqual(child_env["GEMINI_AUTH_TOKEN"], "native-auth")
        self.assertEqual(child_env["HTTP_PROXY"], "http://proxy.invalid")
        for key in ("CODEX_THREAD_ID", "CODEX_SESSION_ID", "CLAUDE_CODE_SESSION_ID"):
            self.assertNotIn(key, child_env)

    def test_duplicate_receive_wait_skips_listener_spawn(self):
        lock_path = self.state / (SID + ".receive.lock")
        fd = wake.private_file(lock_path, os.O_RDWR | os.O_CREAT)
        with os.fdopen(fd, "r+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with patch.object(wake.subprocess, "Popen") as launch:
                self.assertEqual(wake.wait_and_rewake(SID, self.project), 0)
            launch.assert_not_called()

    def test_host_exit_cancels_listener_without_wake(self):
        process = self.child([None, None])
        with patch.object(wake.subprocess, "Popen", return_value=process), \
                patch.object(wake.os, "getppid", side_effect=[1234, 1, 1]), \
                patch.object(wake.time, "sleep"), patch.object(wake, "wake_notice") as notify:
            self.assertEqual(wake.wait_and_rewake(SID, self.project), 0)
        process.terminate.assert_called_once()
        notify.assert_not_called()

    def test_lease_expiry_requests_renewal_without_reading_or_claiming_work(self):
        process = Mock(pid=9876)
        process.poll.return_value = None
        process.wait.return_value = 0
        err = io.StringIO()
        with patch.object(wake.subprocess, "Popen", return_value=process) as launch, \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "monotonic", side_effect=[10.0, 12.0]), \
                patch.object(wake.time, "sleep"), patch.object(wake, "wake_notice") as notify:
            with contextlib.redirect_stderr(err):
                self.assertEqual(wake.wait_and_rewake(SID, self.project, lease_seconds=1), 2)
        launch.assert_called_once()
        process.terminate.assert_called_once()
        notify.assert_not_called()
        self.assertIn("renewal only", err.getvalue())
        self.assertIn("No task has been delivered or claimed", err.getvalue())
        self.assertNotIn("check_inbox", err.getvalue())
        self.assertNotIn("waiting items", err.getvalue())
        events = [json.loads(line) for line in (self.state / (SID + ".events.jsonl")).read_text().splitlines()]
        self.assertTrue(any(row["event"] == "receive_renewal_requested" for row in events))

    def test_successive_renewals_start_scoped_children_and_release_lock(self):
        children = [Mock(pid=1001), Mock(pid=1002)]
        for child in children:
            child.poll.return_value = None
            child.wait.return_value = 0
        with patch.object(wake.subprocess, "Popen", side_effect=children) as launch, \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "monotonic", side_effect=[0.0, 2.0, 0.0, 2.0]), \
                patch.object(wake.time, "sleep"), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(wake.wait_and_rewake(SID, self.project, lease_seconds=1), 2)
            self.assertEqual(wake.wait_and_rewake(SID, self.project, lease_seconds=1), 2)
        self.assertEqual(launch.call_count, 2)
        for child, call in zip(children, launch.call_args_list):
            self.assertEqual(call.args[0][:3], [str(self.tincan), "listen", "--once"])
            self.assertEqual(call.kwargs["env"]["TINCAN_SESSION_ID"], SID)
            self.assertEqual(Path(call.kwargs["cwd"]), self.project)
            child.terminate.assert_called_once()
        # Both calls succeeding proves each prior call released the per-session lock.
        lock_fd = wake.private_file(self.state / (SID + ".receive.lock"), os.O_RDWR)
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        finally:
            os.close(lock_fd)

    def test_owner_exit_wins_when_lease_expires(self):
        process = Mock(pid=9876)
        process.poll.return_value = None
        process.wait.return_value = 0
        err = io.StringIO()
        with patch.object(wake.subprocess, "Popen", return_value=process), \
                patch.object(wake.os, "getppid", side_effect=[1234, 1234, 1]), \
                patch.object(wake.time, "monotonic", side_effect=[0.0, 2.0]), \
                patch.object(wake.time, "sleep"), patch.object(wake, "wake_notice") as notify:
            with contextlib.redirect_stderr(err):
                self.assertEqual(wake.wait_and_rewake(SID, self.project, lease_seconds=1), 0)
        process.terminate.assert_called_once()
        notify.assert_not_called()
        self.assertNotIn("renewal only", err.getvalue())

    def test_notice_before_deadline_keeps_normal_wake_behavior(self):
        process = self.child([None, 0, 0])
        with patch.object(wake.subprocess, "Popen", side_effect=self.launch_with_notice(process)), \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "monotonic", side_effect=[0.0, 0.5]), \
                patch.object(wake.time, "sleep"), patch.object(wake, "wake_notice", wraps=wake.wake_notice) as notify:
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(wake.wait_and_rewake(SID, self.project, lease_seconds=1), 2)
        notify.assert_called_once()
        self.assertEqual(notify.call_args.args[0], SID)
        self.assertEqual(notify.call_args.args[4], "native_wait")

    def test_upgrade_only_exit_rearms_until_a_new_session_notice_arrives(self):
        wake.write_json(self.state / (SID + ".seen.json"), {"sequence": 17})
        first = self.child([0, 0], code=0)
        second = self.child([0, 0], code=0)

        def launch(*_args, **_kwargs):
            if launch.calls == 0:
                launch.calls += 1
                return first
            wake.write_json(self.state / (SID + ".signal.json"), {
                "session_id": SID, "count": 1, "sequence": 18,
            })
            launch.calls += 1
            return second
        launch.calls = 0

        with patch.object(wake.subprocess, "Popen", side_effect=launch) as popen, \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "sleep"), \
                contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(wake.wait_and_rewake(SID, self.project), 2)

        self.assertEqual(popen.call_count, 2)
        events = [json.loads(line) for line in
                  (self.state / (SID + ".events.jsonl")).read_text().splitlines()]
        self.assertTrue(any(row["event"] == "receive_wait_rearmed"
                            and row.get("reason") == "no_new_notice" for row in events))
        self.assertEqual(sum(row["event"] == "desktop_rewake" for row in events), 1)

    def test_receive_lease_policy_requires_private_matching_unexpired_scope(self):
        policy = self.state / (SID + ".receive-policy.json")
        wake.write_json(policy, {
            "session_id": SID, "lease_seconds": 30,
            "expires_at_ns": wake.time.time_ns() + 60_000_000_000,
        })
        self.assertEqual(wake.receive_lease_seconds(SID), 30)
        wrong_scope = self.state / (OTHER + ".receive-policy.json")
        wake.write_json(wrong_scope, {
            "session_id": SID, "lease_seconds": 30,
            "expires_at_ns": wake.time.time_ns() + 60_000_000_000,
        })
        with self.assertRaises(ValueError):
            wake.receive_lease_seconds(OTHER)
        wake.write_json(policy, {
            "session_id": SID, "lease_seconds": 30,
            "expires_at_ns": 1,
        })
        self.assertEqual(wake.receive_lease_seconds(SID), wake.RECEIVE_LEASE_SECONDS)

    def test_listener_failure_does_not_wake(self):
        process = self.child([1, 1], code=1)
        with patch.object(wake.subprocess, "Popen", return_value=process), \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake, "wake_notice") as notify:
            self.assertEqual(wake.wait_and_rewake(SID, self.project), 0)
        notify.assert_not_called()

    def test_valid_own_native_notice_returns_wake_code_and_deduplicates_file_surface(self):
        process = self.child([None, 0, 0])
        err = io.StringIO()
        with patch.object(wake.subprocess, "Popen", side_effect=self.launch_with_notice(process)), \
                patch.object(wake.os, "getppid", return_value=1234), \
                patch.object(wake.time, "sleep"):
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
                self.assertEqual(wake.wait_and_rewake(SID, self.project), 2)
        self.assertIn("no work has been claimed", err.getvalue())
        signal_path = self.state / (SID + ".signal.json")
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(wake.wake_notice(
                SID, self.project, signal_path,
                {"file_path": str(signal_path), "event": "change"}, "file_changed"), 0)
        events = [json.loads(line) for line in (self.state / (SID + ".events.jsonl")).read_text().splitlines()]
        wake_events = [row for row in events if row["event"] == "desktop_rewake"]
        skipped = [row for row in events if row["event"] == "wake_skipped"]
        self.assertEqual(len(wake_events), 1)
        self.assertEqual(wake_events[0]["surface"], "native_wait")
        self.assertTrue(any(row.get("reason") == "already_seen" for row in skipped))


if __name__ == "__main__":
    unittest.main()
