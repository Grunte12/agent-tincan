import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "desktop_wake_recovery", Path(__file__).with_name("desktop-wake.py"))
wake = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(wake)

SID = "00000000-0000-4000-8000-000000000091"
OTHER = "00000000-0000-4000-8000-000000000092"


class DesktopRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.root.chmod(0o700)
        self.project = self.root / "project"
        self.project.mkdir()
        self.subproject = self.project / "src"
        self.subproject.mkdir()
        self.registry = self.root / "routes.json"
        self.registry.write_text(json.dumps({
            "version": 1,
            "routes": [{"project": str(self.project), "task": "project", "session_id": SID}],
        }), encoding="utf-8")
        self.registry.chmod(0o600)
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.policy = self.state / "recovery-policy.json"
        self.patches = [patch.object(wake, "REGISTRY", self.registry),
                        patch.object(wake, "STATE", self.state)]
        for item in self.patches:
            item.start()
            self.addCleanup(item.stop)

    def item(self, sid=SID, cwd=None, event="SessionStart", **extra):
        return {
            "session_id": sid,
            "cwd": str(cwd or self.project),
            "transcript_path": "/synthetic/transcripts/" + sid + ".jsonl",
            "hook_event_name": event,
            **extra,
        }

    def write_policy(self, *, enabled=True, excluded_sessions=None,
                     excluded_projects=None, allowed_sessions=None):
        wake.write_json(self.policy, {
            "enabled": enabled,
            "excluded_session_ids": excluded_sessions or [],
            "excluded_projects": excluded_projects or [],
            "allowed_session_ids": allowed_sessions or [],
        })

    def run_recover(self, item):
        with patch("sys.stdin", io.StringIO(json.dumps(item))), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return wake.main("recover")

    def test_excluded_session_returns_before_any_registration_or_process(self):
        self.write_policy(excluded_sessions=[SID])
        before = {p.name for p in self.state.iterdir()}
        item = self.item(cwd=self.root / "missing")
        with patch.object(wake.subprocess, "Popen") as launch, patch.object(wake, "run_listener") as listen:
            self.assertEqual(self.run_recover(item), 0)
        launch.assert_not_called()
        listen.assert_not_called()
        self.assertEqual({p.name for p in self.state.iterdir()}, before)

    def test_excluded_project_subtree_returns_without_registration_or_process(self):
        self.write_policy(excluded_projects=[str(self.project)])
        before = {p.name for p in self.state.iterdir()}
        with patch.object(wake.subprocess, "Popen") as launch, patch.object(wake, "run_listener") as listen:
            self.assertEqual(self.run_recover(self.item(cwd=self.subproject)), 0)
        launch.assert_not_called()
        listen.assert_not_called()
        self.assertEqual({p.name for p in self.state.iterdir()}, before)

    def test_missing_or_disabled_policy_is_inactive(self):
        self.assertFalse(wake.recovery_allowed(self.item()))
        self.assertEqual(self.run_recover(self.item()), 0)
        self.write_policy(enabled=False)
        before = {p.name for p in self.state.iterdir()}
        with patch.object(wake, "run_listener") as listen:
            self.assertEqual(self.run_recover(self.item()), 0)
        listen.assert_not_called()
        self.assertEqual({p.name for p in self.state.iterdir()}, before)

    def test_pilot_policy_allows_only_listed_existing_session(self):
        self.write_policy(allowed_sessions=[SID])
        with patch.object(wake, "run_listener") as listen:
            self.assertEqual(self.run_recover(self.item(event="Stop")), 0)
        listen.assert_called_once_with(SID, self.project)
        listen.reset_mock()
        before = {p.name for p in self.state.iterdir()}
        with patch.object(wake.subprocess, "Popen") as launch:
            self.assertEqual(self.run_recover(self.item(OTHER)), 0)
        launch.assert_not_called()
        self.assertEqual({p.name for p in self.state.iterdir()}, before)

    def test_empty_pilot_list_allows_dynamic_synthetic_host_session(self):
        self.write_policy(allowed_sessions=[])
        new_item = self.item(OTHER, event="SessionStart")
        with patch.object(wake, "run_listener") as listen:
            self.assertEqual(self.run_recover(new_item), 0)
        listen.assert_called_once_with(OTHER, self.project)
        self.assertEqual(wake.discovery.project_for(OTHER, self.registry, self.state), self.project)

    def test_subagent_and_unsupported_context_are_rejected_without_registration(self):
        self.write_policy()
        before = {p.name for p in self.state.iterdir()}
        with self.assertRaises(ValueError):
            self.run_recover(self.item(agent_id="synthetic-child"))
        with self.assertRaises((ValueError, wake.discovery.route.RouteError)):
            self.run_recover(self.item(event="FileChanged"))
        self.assertEqual({p.name for p in self.state.iterdir()}, before)
        self.assertFalse((self.state / "sessions").exists())

    def test_malformed_policy_and_public_policy_file_fail_closed(self):
        self.write_policy()
        wake.write_json(self.policy, {
            "enabled": True, "excluded_session_ids": ["not-a-uuid"],
            "excluded_projects": [], "allowed_session_ids": [],
        })
        with self.assertRaises((ValueError, wake.discovery.route.RouteError)):
            wake.recovery_allowed(self.item())
        self.write_policy()
        self.policy.chmod(0o644)
        with self.assertRaises(ValueError):
            wake.recovery_allowed(self.item())

    def test_run_listener_with_missing_parent_does_not_spawn(self):
        with patch.object(wake.os, "getppid", return_value=1), patch.object(wake.subprocess, "Popen") as launch:
            wake.run_listener(SID, self.project)
        launch.assert_not_called()


if __name__ == "__main__":
    unittest.main()
