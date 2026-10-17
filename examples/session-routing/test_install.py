from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import tomllib
import types
import unittest


SCRIPT = Path(__file__).with_name("install.py")
installer = types.ModuleType("session_router_install")
sys.modules[installer.__name__] = installer
installer.__file__ = str(SCRIPT)
exec(compile(SCRIPT.read_text(encoding="utf-8"), str(SCRIPT), "exec"), installer.__dict__)


class InstallPlanTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=str(Path(tempfile.gettempdir()).resolve()))
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.home = self.root / "fake home with spaces"
        self.home.mkdir(mode=0o700)
        self.binary = self.root / "fake executable"
        self.binary.write_bytes(b"#!/bin/sh\nprintf called > \"$TINCAN_TEST_MARKER\"\n")
        self.registry = self.home / ".config/tincan/host-session-routes.json"
        self._seed_configs()

    def tearDown(self):
        self.temp.cleanup()

    def _seed_configs(self):
        codex = self.home / ".codex"
        codex.mkdir()
        (codex / "config.toml").write_text('''model = "keep-model"
[mcp_servers.agent-tincan]
command = "old-binary"
args = ["mcp"]
[mcp_servers.agent-tincan.env]
TINCAN_SESSION_ID = "inherited-placeholder"
KEEP_EXISTING = "codex-value"
[other]
preserve = "codex-section"
''')
        (codex / "hooks.json").write_text(json.dumps({"hooks": {
            "UserPromptSubmit": [{"command": "keep-codex-hook"}],
        }}, indent=2))
        cursor = self.home / ".cursor"
        cursor.mkdir()
        (cursor / "mcp.json").write_text(json.dumps({"mcpServers": {
            "agent-tincan": {"command": "old-cursor", "args": ["mcp"], "env": {"KEEP_EXISTING": "cursor-value", "TINCAN_SESSION_ID": "placeholder"}},
            "other-server": {"command": "keep-cursor", "args": ["other"]},
        }}, indent=2))
        (cursor / "hooks.json").write_text(json.dumps({"version": 1, "hooks": {
            "unrelated": [{"command": "keep-cursor-hook"}],
            "preToolUse": [{"command": "keep-cursor-tool-hook", "matcher": "other"}],
        }}, indent=2))
        gemini = self.home / ".gemini/config"
        gemini.mkdir(parents=True)
        (gemini / "mcp_config.json").write_text(json.dumps({"mcpServers": {
            "agent-tincan": {"command": "old-agy", "args": ["mcp"], "env": {"KEEP_EXISTING": "agy-value", "TINCAN_SESSION_ID": "placeholder"}},
            "other-server": {"command": "keep-agy", "args": ["other"]},
        }}, indent=2))
        (gemini / "hooks.json").write_text(json.dumps({
            "other-hook": {"BeforeAgent": [{"command": "keep-agy-hook"}]},
        }, indent=2))

    def plan(self, preservation=None):
        return installer.plan(self.home, Path(sys.executable), self.binary, preservation)

    def test_missing_or_nonexecutable_python_is_rejected_before_planning(self):
        missing = self.root / 'missing-python'
        nonexecutable = self.root / 'not-executable'
        nonexecutable.write_text('not an interpreter')
        nonexecutable.chmod(0o600)
        for python in (missing, nonexecutable, Path('python3')):
            with self.subTest(python=python), self.assertRaisesRegex(ValueError, 'existing_executable_python_required'):
                installer.plan(self.home, python, self.binary)
        self.assertFalse(self.registry.exists())

    def test_plan_preserves_native_config_and_sets_host_routes(self):
        writes, _ = self.plan()
        codex_path = self.home / ".codex/config.toml"
        codex = tomllib.loads(writes[codex_path][0].decode())
        codex_server = codex["mcp_servers"]["agent-tincan"]
        self.assertEqual(codex_server["command"], str(self.home / ".local/bin/tincan-session-routing-5"))
        self.assertEqual(codex_server["args"], ["mcp"])
        self.assertEqual(codex_server["env"]["TINCAN_SESSION_ID"], "")
        self.assertEqual(codex_server["env"]["KEEP_EXISTING"], "codex-value")
        self.assertEqual(codex["model"], "keep-model")
        self.assertEqual(codex["other"]["preserve"], "codex-section")

        expected_hosts = {"codex": "codex", "cursor": "cursor", "antigravity": "antigravity", "linkhub-muse": "muse"}
        self.assertEqual(json.loads(codex_server["env"]["TINCAN_SESSION_HOSTS"]), expected_hosts)
        for host, relative, kind in (
            ("cursor", ".cursor/mcp.json", "cursor"),
            ("antigravity", ".gemini/config/mcp_config.json", "antigravity"),
        ):
            data = json.loads(writes[self.home / relative][0])
            server = data["mcpServers"]["agent-tincan"]
            self.assertEqual(server["command"], str(self.home / ".local/bin/tincan-session-routing-5"))
            self.assertEqual(server["args"], ["mcp"])
            self.assertEqual(server["env"]["TINCAN_SESSION_ID"], "")
            self.assertEqual(server["env"]["KEEP_EXISTING"], "cursor-value" if host == "cursor" else "agy-value")
            self.assertEqual(json.loads(server["env"]["TINCAN_SESSION_HOSTS"]), expected_hosts)
            self.assertEqual(server["env"]["TINCAN_HOST_KIND"], kind)
            self.assertEqual(data["mcpServers"]["other-server"]["command"], "keep-cursor" if host == "cursor" else "keep-agy")

    def test_hook_schemas_retain_unrelated_entries_and_are_idempotent(self):
        first, _ = self.plan()
        second, _ = self.plan()
        self.assertEqual(first, second)
        codex = json.loads(first[self.home / ".codex/hooks.json"][0])
        self.assertIn("SessionStart", codex["hooks"])
        self.assertEqual(codex["hooks"]["UserPromptSubmit"][0], {"command": "keep-codex-hook"})
        self.assertEqual(len(codex["hooks"]["UserPromptSubmit"]), 2)
        codex_text = json.dumps(codex)
        self.assertNotIn("trustBypass", codex_text)
        self.assertNotIn("skip-permissions", codex_text)

        cursor = json.loads(first[self.home / ".cursor/hooks.json"][0])
        self.assertEqual(cursor["hooks"]["unrelated"], [{"command": "keep-cursor-hook"}])
        self.assertEqual(cursor["hooks"]["preToolUse"][0]["command"], "keep-cursor-tool-hook")
        self.assertTrue(any(row.get("failClosed") is True for row in cursor["hooks"]["preToolUse"]))

        agy = json.loads(first[self.home / ".gemini/config/hooks.json"][0])
        self.assertEqual(agy["other-hook"], {"BeforeAgent": [{"command": "keep-agy-hook"}]})
        self.assertEqual(agy["tincan-native-session-routing"]["PreToolUse"][0]["matcher"], "(^|[_:.-])agent[-_]tincan([_:.-]|$)|^call_mcp_tool$")
        agy_text = json.dumps(agy)
        self.assertNotIn("permissionOverrides", agy_text)
        self.assertNotIn("allTools", agy_text)

    def test_router_intent_alias_replaces_only_existing_adapter_hooks(self):
        cursor_command = '/opt/tincan/router-intent.py hook --host cursor --agent cursor'
        cursor = installer.hooks({"hooks": {
            "sessionStart": [{"command": "/old/router-intent.py hook --host cursor --agent cursor"},
                             {"command": "keep-cursor-hook"}],
        }}, "cursor", cursor_command)
        rows = cursor["hooks"]["sessionStart"]
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["command"], "keep-cursor-hook")
        self.assertEqual(sum("router-intent.py" in row.get("command", "") for row in rows), 1)

        agy_command = '/opt/tincan/router-intent.py hook --host antigravity --agent antigravity'
        agy = installer.hooks({"tincan-native-session-routing": {
            "PreInvocation": [{"type": "command", "command": "/old/router-intent.py hook --host antigravity --agent antigravity"}],
        }}, "antigravity", agy_command)
        self.assertEqual(len(agy["tincan-native-session-routing"]["PreInvocation"]), 1)
        self.assertEqual(agy["tincan-native-session-routing"]["PreInvocation"][0]["command"],
                         agy_command + " --event PreInvocation")

    def test_only_fully_verified_passed_owner_pointers_are_preserved(self):
        passed = {"agent": "codex", "passed": True, "claim_scope_verified": True,
                  "reply_scope_verified": True, "owner_receipt_verified": True,
                  "recipient_session": "00000000-0000-4000-8000-000000000001"}
        false_passed = {**passed, "recipient_session": "00000000-0000-4000-8000-000000000002", "passed": False}
        incomplete = {**passed, "recipient_session": "00000000-0000-4000-8000-000000000003", "reply_scope_verified": False}
        unknown = {**passed, "agent": "unknown-host", "recipient_session": "00000000-0000-4000-8000-000000000004"}
        writes, _ = self.plan({"tests": [passed, false_passed, incomplete, unknown]})
        state = json.loads(writes[self.registry][0])
        self.assertEqual(state["owners"], [])
        self.assertEqual(state["sessions"], [])
        self.assertEqual(state["preserved"], [{"agent": "codex", "host": "codex", "session_id": passed["recipient_session"]}])

    def test_apply_copies_fake_binary_without_executing_it_and_leaves_existing_registry(self):
        self.registry.parent.mkdir(parents=True, mode=0o700)
        original_registry = b'{"version":1,"owners":[],"sessions":[],"preserved":[]}\n'
        self.registry.write_bytes(original_registry)
        self.registry.chmod(0o600)
        writes, _ = self.plan()
        marker = self.root / "binary-was-executed"
        os.environ["TINCAN_TEST_MARKER"] = str(marker)
        try:
            installer.apply(writes, self.root / "backups")
        finally:
            os.environ.pop("TINCAN_TEST_MARKER", None)
        self.assertFalse(marker.exists())
        self.assertEqual(self.registry.read_bytes(), original_registry)

    def test_apply_rejects_symlink_target(self):
        real = self.root / "real-target"
        real.write_text("keep")
        link = self.root / "link-target"
        link.symlink_to(real)
        with self.assertRaisesRegex(ValueError, "refusing_symlink_target"):
            installer.apply({link: (b"replace", 0o600)}, self.root / "backup")
        self.assertEqual(real.read_text(), "keep")


if __name__ == "__main__":
    unittest.main()
