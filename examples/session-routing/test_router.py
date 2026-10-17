from __future__ import annotations

import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import types
import unittest


SCRIPT = Path(__file__).with_name("router.py")
router = types.ModuleType("router")
sys.modules[router.__name__] = router
router.__file__ = str(SCRIPT)
exec(compile(SCRIPT.read_text(encoding="utf-8"), str(SCRIPT), "exec"), router.__dict__)


class RouterTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=str(Path(tempfile.gettempdir()).resolve()))
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.project = self.root / "project"
        self.project.mkdir()
        self.other = self.root / "other"
        self.other.mkdir()
        self.registry = self.root / "private" / "routes.json"
        self.registry.parent.mkdir(mode=0o700)
        self.registry.parent.chmod(0o700)
        self.key = self.root / "private" / "key"
        router.key_bytes(self.key, create=True)

    def tearDown(self):
        self.temp.cleanup()

    def test_native_event_contracts_and_outputs(self):
        codex = router.hook("codex", "agent", {"hook_event_name": "SessionStart", "session_id": "codex1", "cwd": str(self.project)}, self.registry, self.key)
        self.assertEqual(codex, {})
        cursor = router.hook("cursor", "agent", {"hook_event_name": "sessionStart", "conversation_id": "cursor1", "workspace_roots": [str(self.project)]}, self.registry, self.key)
        self.assertIn("host_context", cursor["additional_context"])
        agy = router.hook("antigravity", "agent", {"conversationId": "agy1", "workspacePaths": [str(self.project)]}, self.registry, self.key, "PreInvocation")
        self.assertIn("injectSteps", agy)
        muse = router.hook("muse", "agent", {"sessionId": "muse1", "cwd": str(self.project)}, self.registry, self.key)
        self.assertIn("host_context", muse)
        # Cursor generation IDs are not a persistent conversation identity.
        with self.assertRaisesRegex(router.Error, "invalid_identity"):
            router.event_identity("cursor", {"hook_event_name": "sessionStart", "generation_id": "g1", "workspace_roots": [str(self.project)]})

    def test_preserved_owner_migrates_only_on_matching_native_session(self):
        state = {"version": 1, "owners": [], "sessions": [], "preserved": [{"agent": "agent", "host": "codex", "session_id": "old"}]}
        self.registry.write_text(json.dumps(state)); self.registry.chmod(0o600)
        with self.assertRaisesRegex(router.Error, "preserved_owner_needs_native_discovery"):
            router.update(self.registry, "agent", "codex", str(self.project))
        row = router.update(self.registry, "agent", "codex", str(self.project), "old")
        self.assertEqual(row["session_id"], "old")
        self.assertEqual(router.update(self.registry, "agent", "codex", str(self.other), "old")["project"], str(self.project))

    def test_concurrent_registrations_are_serialized(self):
        errors = []
        barrier = threading.Barrier(8)
        def register(i):
            try:
                barrier.wait()
                router.update(self.registry, "agent", "cursor", str(self.project), f"s{i}")
            except Exception as exc:  # surfaced in the parent test thread
                errors.append(exc)
        threads = [threading.Thread(target=register, args=(i,)) for i in range(8)]
        for t in threads: t.start()
        for t in threads: t.join()
        self.assertEqual(errors, [])
        fd = os.open(self.registry.parent, os.O_RDONLY)
        try:
            state = router.load(fd, self.registry.name)
        finally:
            os.close(fd)
        self.assertEqual({r["session_id"] for r in state["sessions"]}, {f"s{i}" for i in range(8)})

    def test_resolution_requires_exactly_one_discovered_session(self):
        with self.assertRaisesRegex(router.Error, "existing_session_not_discovered"):
            router.update(self.registry, "agent", "codex", str(self.project))
        router.update(self.registry, "agent", "codex", str(self.project), "s1")
        self.assertEqual(router.update(self.registry, "agent", "codex", str(self.project))["session_id"], "s1")
        router.update(self.registry, "other-agent", "codex", str(self.project), "s1")
        router.update(self.registry, "other-agent", "codex", str(self.project), "s2")
        with self.assertRaisesRegex(router.Error, "owner_ambiguous"):
            router.update(self.registry, "other-agent", "codex", str(self.project))

    def test_existing_session_owner_survives_cwd_change(self):
        first = router.update(self.registry, "agent", "muse", str(self.project), "session")
        second = router.update(self.registry, "agent", "muse", str(self.other), "session")
        self.assertEqual(first["project"], second["project"])

    def test_existing_cursor_session_keeps_original_project_with_multiple_roots(self):
        router.key_bytes(self.key, create=True)
        original = router.update(self.registry, "agent", "cursor", str(self.project), "stable-session")
        result = router.hook("cursor", "agent", {
            "hook_event_name": "sessionStart", "conversation_id": "stable-session",
            "workspace_roots": [str(self.project), str(self.other)],
        }, self.registry, self.key)
        token = result["additional_context"].split("host_context for this conversation: ", 1)[1].split(". Include it", 1)[0]
        claims = router.verify(token, self.key, "agent", "cursor")
        self.assertEqual(claims["project"], original["project"])

    def test_unrelated_mcp_hooks_bypass_registry_and_key(self):
        missing_registry = self.root / "absent" / "routes.json"
        missing_key = self.root / "absent" / "key"
        cursor_before_mcp = router.hook("cursor", "agent", {
            "hook_event_name": "beforeMCPExecution", "mcp_server_name": "other-server",
            "conversation_id": "synthetic", "workspace_roots": [str(self.project)],
        }, missing_registry, missing_key)
        self.assertEqual(cursor_before_mcp, {"permission": "allow"})
        cursor_pre_tool = router.hook("cursor", "agent", {
            "hook_event_name": "preToolUse", "mcp_server_name": "filesystem",
            "tool_name": "filesystem.read", "conversation_id": "synthetic",
            "workspace_roots": [str(self.project)],
        }, missing_registry, missing_key)
        self.assertEqual(cursor_pre_tool, {"permission": "allow"})
        agy = router.hook("antigravity", "agent", {
            "toolCall": {"name": "filesystem.read", "args": {}},
        }, missing_registry, missing_key, "PreToolUse")
        self.assertEqual(agy, {"decision": "deny_unless_prior_grant"})

    def test_owner_is_immutable_and_routes_are_isolated_by_agent_and_host(self):
        router.update(self.registry, "agent", "codex", str(self.project), "owner", bind=True)
        with self.assertRaisesRegex(router.Error, "owner_conflict_immutable"):
            router.update(self.registry, "agent", "codex", str(self.project), "second", bind=True)
        for agent, host, sid in (("other-agent", "codex", "other-agent-session"), ("agent", "cursor", "cursor-session")):
            self.assertEqual(router.update(self.registry, agent, host, str(self.project), sid)["session_id"], sid)

    def test_rejects_invalid_identities_hosts_workspaces_and_subagents(self):
        for bad in ("", "has space", "../escape", "x" * 129):
            with self.subTest(identity=bad), self.assertRaises(router.Error):
                router.identity(bad)
        with self.assertRaisesRegex(router.Error, "unsupported_host"):
            router.update(self.registry, "agent", "claude", str(self.project), "s")
        with self.assertRaisesRegex(router.Error, "absolute_project_required"):
            router.update(self.registry, "agent", "codex", "relative", "s")
        for event in ({"agent_id": "child"}, {"subagent_id": "child"}, {"is_subagent": True}):
            with self.subTest(event=event), self.assertRaisesRegex(router.Error, "main_conversation_required"):
                router.event_identity("codex", {**event, "hook_event_name": "SessionStart", "session_id": "s", "cwd": str(self.project)})
        with self.assertRaisesRegex(router.Error, "workspace_ambiguous"):
            router.event_identity("cursor", {"hook_event_name": "sessionStart", "conversation_id": "s", "workspace_roots": [str(self.project), str(self.other)]})
        with self.assertRaisesRegex(router.Error, "host_identity_conflict"):
            router.event_identity("cursor", {"hook_event_name": "sessionStart", "conversation_id": "s", "session_id": "other", "workspace_roots": [str(self.project)]})

    def test_registry_rejects_duplicate_entries_and_private_file_violations(self):
        row = {"agent": "agent", "host": "codex", "project": str(self.project), "session_id": "s"}
        bad_state = {"version": 1, "owners": [row, row], "sessions": [], "preserved": []}
        self.registry.write_text(json.dumps(bad_state)); self.registry.chmod(0o600)
        with self.assertRaisesRegex(router.Error, "duplicate_host_route"):
            router.update(self.registry, "agent", "codex", str(self.project), "s")
        self.registry.unlink()
        self.registry.symlink_to(self.root / "attacker")
        (self.root / "attacker").write_text("{}")
        with self.assertRaises(router.Error):
            router.update(self.registry, "agent", "codex", str(self.project), "s")
        self.registry.unlink()
        self.registry.write_text(json.dumps({"version": 1, "owners": [], "sessions": [], "preserved": []})); self.registry.chmod(0o644)
        with self.assertRaises(router.Error):
            router.update(self.registry, "agent", "codex", str(self.project), "s")

    def test_signed_context_rejects_wrong_host_agent_expiry_and_session(self):
        router.key_bytes(self.key, create=True)
        row = {"agent": "agent", "host": "cursor", "project": str(self.project), "session_id": "s1"}
        signed = router.token(row, self.key, now=100)
        self.assertEqual(router.verify(signed, self.key, "agent", "cursor", now=101)["session_id"], "s1")
        for agent, host, now in (("other", "cursor", 101), ("agent", "codex", 101), ("agent", "cursor", 86500)):
            with self.subTest(agent=agent, host=host, now=now), self.assertRaisesRegex(router.Error, "invalid_host_context"):
                router.verify(signed, self.key, agent, host, now=now)
        router.update(self.registry, "agent", "cursor", str(self.project), "s2")
        result = router.hook("cursor", "agent", {"hook_event_name": "beforeMCPExecution", "conversation_id": "s2", "workspace_roots": [str(self.project)], "mcp_server_name": "agent-tincan", "tool_input": {"host_context": signed}}, self.registry, self.key)
        self.assertEqual(result["permission"], "deny")

    def test_cursor_tool_input_rewrite_preserves_native_fields(self):
        router.update(self.registry, "agent", "cursor", str(self.project), "cursor-session")
        result = router.hook("cursor", "agent", {
            "hook_event_name": "preToolUse", "conversation_id": "cursor-session",
            "workspace_roots": [str(self.project)], "tool_name": "agent-tincan__check_inbox",
            "request_id": "request-1", "tool_input": {"body": "keep this", "limit": 3},
        }, self.registry, self.key)
        self.assertEqual(result["permission"], "allow")
        self.assertEqual(result["updated_input"]["body"], "keep this")
        self.assertEqual(result["updated_input"]["limit"], 3)
        self.assertIn("host_context", result["updated_input"])

    def test_antigravity_pretooluse_never_grants_new_permissions(self):
        router.update(self.registry, "agent", "antigravity", str(self.project), "agy-session")
        wrong_session = router.token({"agent": "agent", "host": "antigravity", "project": str(self.project), "session_id": "other-session"}, self.key)

        def invoke(context):
            return router.hook("antigravity", "agent", {
                "conversationId": "agy-session", "workspacePaths": [str(self.project)],
                "toolCall": {"name": "agent-tincan_check_inbox", "args": context},
            }, self.registry, self.key, "PreToolUse")

        with_context = invoke({"host_context": router.token({"agent": "agent", "host": "antigravity", "project": str(self.project), "session_id": "agy-session"}, self.key)})
        self.assertEqual(with_context, {"decision": "deny_unless_prior_grant"})
        self.assertNotIn("permissionOverrides", with_context)
        self.assertEqual(invoke({"host_context": wrong_session})["decision"], "deny")
        self.assertEqual(invoke({})["decision"], "deny")

    def test_cursor_cli_bare_mcp_name_injects_model_context_without_leaking_arguments(self):
        event = {'hook_event_name': 'preToolUse', 'conversation_id': 'cursor-session',
                 'workspace_roots': [str(self.project)], 'tool_name': 'MCP:claim',
                 'tool_input': {'request_id': 'probe'}}
        result = router.hook('cursor', 'agent', event, self.registry, self.key)
        self.assertEqual(result['permission'], 'allow')
        self.assertIn('host_context', result['additional_context'])
        self.assertNotIn('updated_input', result)
        denied = router.hook('cursor', 'agent', {
            **event, 'hook_event_name': 'beforeMCPExecution', 'mcp_server_name': 'agent-tincan',
            'tool_input': json.dumps(event['tool_input']),
        }, self.registry, self.key)
        self.assertEqual(denied['permission'], 'deny')
        self.assertEqual(router.hook('cursor', 'agent', event, self.root/'absent/routes', self.root/'absent/key'), {'permission': 'allow'})

    def test_antigravity_native_mcp_wrapper_checks_actual_chat_and_keeps_other_grants(self):
        correct = router.token({'agent': 'agent', 'host': 'antigravity', 'project': str(self.project), 'session_id': 'agy-session'}, self.key)
        wrong = router.token({'agent': 'agent', 'host': 'antigravity', 'project': str(self.project), 'session_id': 'other-session'}, self.key)
        def invoke(context, server='agent-tincan', as_string=False):
            args = {'request_id': 'probe', **context}
            return router.hook('antigravity', 'agent', {
                'conversationId': 'agy-session', 'workspacePaths': [str(self.project)],
                'toolCall': {'name': 'call_mcp_tool', 'args': {
                    'ServerName': server, 'ToolName': 'claim',
                    'Arguments': json.dumps(args) if as_string else args}},
            }, self.registry, self.key, 'PreToolUse')
        for as_string in (True, False):
            self.assertEqual(invoke({'host_context': correct}, as_string=as_string), {'decision': 'deny_unless_prior_grant'})
            self.assertEqual(invoke({'host_context': wrong}, as_string=as_string)['decision'], 'deny')
            self.assertEqual(invoke({}, as_string=as_string)['decision'], 'deny')
        self.assertEqual(invoke({}, server='other-server'), {'decision': 'deny_unless_prior_grant'})


if __name__ == "__main__":
    unittest.main()
