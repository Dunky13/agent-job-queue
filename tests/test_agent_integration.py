"""Real isolated instruction-file flows, including unsafe and concurrent edits."""
from __future__ import annotations

import fcntl
import json
import os
import shlex
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import agent_integration as integration

ROOT = Path(__file__).resolve().parents[1]


class AgentIntegrationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="agent-integration-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.home = self.root / "home with 'quote"
        self.home.mkdir(mode=0o700)
        self.environment = mock.patch.dict(os.environ, {"HOME": str(self.home)})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        for variable in ("CODEX_HOME", "CLAUDE_CONFIG_DIR"):
            os.environ.pop(variable, None)
        self.codex = self.home / ".codex/AGENTS.md"
        self.claude = self.home / ".claude/CLAUDE.md"

    def write(self, path, data):
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        path.write_bytes(data)
        return path

    def row(self, runner):
        return next(row for row in integration.snapshot()["runners"] if row["id"] == runner)

    def cli(self, *arguments):
        return subprocess.run([sys.executable, str(ROOT / "agent_integration.py"), *arguments],
                              env=os.environ.copy(), capture_output=True, text=True, check=False, timeout=5)

    def test_status_is_read_only_when_no_directories_exist(self):
        before = list(self.home.iterdir())
        state = integration.snapshot()
        self.assertEqual(list(self.home.iterdir()), before)
        self.assertEqual(len(state["runners"]), 2)
        for row in state["runners"]:
            self.assertFalse(row["configured"])
            self.assertIsNone(row["error"])
        self.assertEqual(self.row("codex")["path"], str(self.codex))
        self.assertEqual(self.row("claude")["path"], str(self.claude))

    def test_instruction_quotes_actual_installed_command_path(self):
        content = integration.instruction()
        command = next(line for line in content.splitlines() if " run --label " in line)
        self.assertEqual(shlex.split(command)[0], str(self.home / ".local/bin/agent-heavy"))
        self.assertIn("Do not change the operator's parallel job limit", content)
        self.assertIn("do not nest", content)
        self.assertIn("stdin receives EOF", content)

    def test_install_is_opt_in_per_runner_and_idempotent(self):
        integration.change("codex", "install")
        installed = self.codex.read_bytes()
        self.assertTrue(self.row("codex")["configured"])
        self.assertFalse(self.claude.exists())
        self.assertEqual(stat.S_IMODE(self.codex.stat().st_mode), 0o600)
        before = self.codex.stat()
        integration.change("codex", "install")
        self.assertEqual(self.codex.read_bytes(), installed)
        self.assertEqual(self.codex.stat().st_ino, before.st_ino)
        self.assertEqual(self.codex.stat().st_mtime_ns, before.st_mtime_ns)
        self.assertEqual(list(self.codex.parent.glob("*.agent-job-queue-backup-*")), [])

    def test_append_remove_preserves_every_unrelated_byte_and_file_mode(self):
        originals = (b"", b"no final newline", b"import shared.md\n", b"# Heading\r\n\r\nPrivate text\r\n", b"\xff\xfe\x00")
        for original in originals:
            with self.subTest(original=original):
                self.write(self.claude, original).chmod(0o640)
                integration.change("claude", "install")
                self.assertTrue(self.claude.read_bytes().startswith(original))
                self.assertTrue(self.row("claude")["configured"])
                self.assertEqual(stat.S_IMODE(self.claude.stat().st_mode), 0o640)
                integration.change("claude", "remove")
                self.assertEqual(self.claude.read_bytes(), original)
                self.assertFalse(self.row("claude")["configured"])

    def test_backups_exist_before_replacement_and_are_private(self):
        original = b"Private unrelated content\r\n"
        self.write(self.claude, original)
        real_replace = os.replace

        def replace(*args, **kwargs):
            backups = list(self.claude.parent.glob("CLAUDE.md.agent-job-queue-backup-*"))
            self.assertEqual(len(backups), 1)
            self.assertEqual(backups[0].read_bytes(), original)
            self.assertEqual(stat.S_IMODE(backups[0].stat().st_mode), 0o600)
            return real_replace(*args, **kwargs)

        with mock.patch.object(integration.os, "replace", side_effect=replace):
            integration.change("claude", "install")

    def test_remove_absent_integration_does_not_create_any_files(self):
        integration.change("codex", "remove")
        self.assertFalse(self.codex.parent.exists())
        original = b"Existing custom shared instructions\n"
        self.write(self.claude, original)
        before = set(self.claude.parent.iterdir())
        integration.change("claude", "remove")
        self.assertEqual(self.claude.read_bytes(), original)
        self.assertEqual(set(self.claude.parent.iterdir()), before)

    def test_existing_shared_import_is_not_claimed_or_modified(self):
        original = b"Read shared agent-heavy rules in shared.md\n@shared.md\n"
        self.write(self.codex, original)
        self.assertFalse(self.row("codex")["configured"])
        self.assertEqual(self.codex.read_bytes(), original)
        integration.change("codex", "install")
        integration.change("codex", "remove")
        self.assertEqual(self.codex.read_bytes(), original)

    def test_unrelated_edits_before_and_after_block_survive_removal(self):
        self.write(self.claude, b"Original")
        integration.change("claude", "install")
        self.claude.write_bytes(b"New prefix\r\n" + self.claude.read_bytes() + b"\r\nNew suffix")
        integration.change("claude", "remove")
        self.assertEqual(self.claude.read_bytes(), b"New prefix\r\nOriginal\r\nNew suffix")

    def test_malformed_edited_duplicate_blocks_are_refused_without_content_disclosure(self):
        integration.change("claude", "install")
        valid = self.claude.read_bytes()
        bad_values = (valid.replace(b"conservatively", b"SECRET"), valid + valid,
                      valid.replace(integration.END, b""), integration.END,
                      b"<!-- agent-job-queue:begin v9 -->\nSECRET\n")
        for damaged in bad_values:
            with self.subTest(damaged=damaged[:60]):
                self.claude.write_bytes(damaged)
                row = self.row("claude")
                self.assertFalse(row["configured"])
                self.assertIn("edited", row["error"])
                self.assertNotIn("SECRET", row["error"])
                for action in ("install", "remove"):
                    with self.assertRaises(integration.IntegrationError):
                        integration.change("claude", action)
                    self.assertEqual(self.claude.read_bytes(), damaged)

    def test_modified_owned_separator_is_refused(self):
        self.write(self.claude, b"Existing")
        integration.change("claude", "install")
        self.claude.write_bytes(self.claude.read_bytes().replace(b"Existing\n\n", b"Existing\n"))
        with self.assertRaises(integration.IntegrationError):
            integration.change("claude", "remove")

    def test_custom_absolute_agent_directories(self):
        for variable, runner, filename in (("CODEX_HOME", "codex", "AGENTS.md"),
                                           ("CLAUDE_CONFIG_DIR", "claude", "CLAUDE.md")):
            target = self.home / (runner + " config")
            os.environ[variable] = str(target)
            integration.change(runner, "install")
            self.assertTrue((target / filename).exists())
            self.assertEqual(self.row(runner)["path"], str(target / filename))
        self.assertFalse(self.codex.parent.exists())
        self.assertFalse(self.claude.parent.exists())

    def test_relative_empty_or_parent_traversal_overrides_are_refused(self):
        for value in ("relative", "", str(self.home / "nested/../elsewhere")):
            with self.subTest(value=value):
                os.environ["CODEX_HOME"] = value
                self.assertIsNotNone(self.row("codex")["error"])
                with self.assertRaises(integration.IntegrationError):
                    integration.change("codex", "install")
        self.assertEqual(list(self.home.iterdir()), [])

    def test_codex_whitespace_only_override_stays_unchanged_and_global_rules_remain_active(self):
        whitespace_values = (b" \n", "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005"
                             "\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000".encode())
        for whitespace in whitespace_values:
            with self.subTest(whitespace=whitespace):
                original = b"Base shared/private instructions\r\n"
                self.write(self.codex, original)
                override = self.write(self.codex.with_name("AGENTS.override.md"), whitespace)
                self.assertEqual(self.row("codex")["path"], str(self.codex))
                integration.change("codex", "install")
                self.assertTrue(self.row("codex")["configured"])
                self.assertEqual(self.row("codex")["path"], str(self.codex))
                self.assertTrue(self.codex.read_bytes().startswith(original))
                self.assertEqual(override.read_bytes(), whitespace)
                integration.change("codex", "remove")
                self.assertEqual(self.codex.read_bytes(), original)
                self.assertEqual(override.read_bytes(), whitespace)

    def test_codex_nonwhitespace_override_matches_lossy_utf8_and_rust_trim(self):
        nonwhitespace_values = (b"Actual override", b"\x1c", b"\x1d", b"\x1e", b"\x1f", b"\xff", b"\x00",
                                "\u200b".encode(), "\ufeff".encode())
        for content in nonwhitespace_values:
            with self.subTest(content=content):
                original = b"Base instructions must stay untouched"
                self.write(self.codex, original)
                override = self.write(self.codex.with_name("AGENTS.override.md"), content)
                self.assertEqual(self.row("codex")["path"], str(override))
                integration.change("codex", "install")
                self.assertTrue(self.row("codex")["configured"])
                self.assertEqual(self.row("codex")["path"], str(override))
                self.assertEqual(self.codex.read_bytes(), original)
                integration.change("codex", "remove")
                self.assertEqual(override.read_bytes(), content)
                self.assertEqual(self.codex.read_bytes(), original)

    def test_codex_empty_override_uses_regular_global_file(self):
        override = self.write(self.codex.with_name("AGENTS.override.md"), b"")
        integration.change("codex", "install")
        self.assertEqual(self.row("codex")["path"], str(self.codex))
        self.assertEqual(override.read_bytes(), b"")

    def test_codex_oversized_instructions_not_claimed_configured(self):
        self.write(self.codex, b"x" * integration.CODEX_MAX_BYTES)
        with self.assertRaisesRegex(integration.IntegrationError, "32 KiB"):
            integration.change("codex", "install")
        self.assertEqual(self.codex.read_bytes(), b"x" * integration.CODEX_MAX_BYTES)
        self.codex.write_bytes(b"")
        integration.change("codex", "install")
        self.codex.write_bytes(self.codex.read_bytes() + b"x" * integration.CODEX_MAX_BYTES)
        row = self.row("codex")
        self.assertFalse(row["configured"])
        self.assertIn("32 KiB", row["error"])
        integration.change("codex", "remove")
        self.assertEqual(self.codex.read_bytes(), b"x" * integration.CODEX_MAX_BYTES)

    def test_symlink_target_and_parent_and_override_are_refused(self):
        outside = self.write(self.root / "outside.txt", b"Do not edit")
        self.claude.parent.mkdir(mode=0o700)
        self.claude.symlink_to(outside)
        self.assertIsNotNone(self.row("claude")["error"])
        with self.assertRaises(OSError):
            integration.change("claude", "install")
        self.claude.unlink()
        (self.claude.parent / ".agent-job-queue.lock").unlink()
        self.claude.parent.rmdir()
        other = self.root / "outside-dir"
        other.mkdir(mode=0o700)
        self.claude.parent.symlink_to(other, target_is_directory=True)
        with self.assertRaises(OSError):
            integration.change("claude", "install")
        self.assertEqual(list(other.iterdir()), [])
        self.codex.parent.mkdir(mode=0o700)
        self.codex.with_name("AGENTS.override.md").symlink_to(outside)
        self.assertIsNotNone(self.row("codex")["error"])
        self.assertEqual(outside.read_bytes(), b"Do not edit")

    def test_nonregular_hardlinked_and_writable_targets_are_refused(self):
        self.claude.parent.mkdir(mode=0o700)
        os.mkfifo(self.claude)
        self.assertIsNotNone(self.row("claude")["error"])
        self.claude.unlink()
        self.write(self.claude, b"Shared inode")
        os.link(self.claude, self.claude.parent / "another-name")
        with self.assertRaises(integration.IntegrationError):
            integration.change("claude", "install")
        (self.claude.parent / "another-name").unlink()
        self.claude.chmod(0o666)
        with self.assertRaises(integration.IntegrationError):
            integration.change("claude", "install")

    def test_nonowned_target_is_refused(self):
        self.write(self.claude, b"Another owner's file")
        inode = self.claude.stat().st_ino
        real_fstat = os.fstat

        def foreign_owner(descriptor):
            info = real_fstat(descriptor)
            if info.st_ino == inode:
                values = list(info)
                values[4] = os.getuid() + 1
                return os.stat_result(values)
            return info

        with mock.patch.object(integration.os, "fstat", side_effect=foreign_owner):
            with self.assertRaises(integration.IntegrationError):
                integration.change("claude", "install")

    def test_group_writable_parent_is_refused(self):
        self.claude.parent.mkdir(mode=0o700)
        self.claude.parent.chmod(0o770)
        try:
            self.assertIsNotNone(self.row("claude")["error"])
            with self.assertRaises(integration.IntegrationError):
                integration.change("claude", "install")
        finally:
            self.claude.parent.chmod(0o700)
        self.assertEqual(list(self.claude.parent.iterdir()), [])

    def test_mutations_lock_and_refuse_unsafe_lock(self):
        self.claude.parent.mkdir(mode=0o700)
        lock = self.claude.parent / ".agent-job-queue.lock"
        with lock.open("w") as stream:
            lock.chmod(0o600)
            fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(integration.IntegrationError, "Another"):
                integration.change("claude", "install")
        self.assertFalse(self.claude.exists())
        lock.chmod(0o644)
        with self.assertRaisesRegex(integration.IntegrationError, "Unsafe"):
            integration.change("claude", "install")

    def test_concurrent_file_change_is_not_overwritten(self):
        self.write(self.claude, b"Original")
        real_write = integration._private_write

        def concurrent_write(descriptor, name, data, mode=0o600):
            real_write(descriptor, name, data, mode)
            if name.endswith(".tmp"):
                self.claude.write_bytes(b"Concurrent human edit")

        with mock.patch.object(integration, "_private_write", side_effect=concurrent_write):
            with self.assertRaisesRegex(integration.IntegrationError, "concurrently"):
                integration.change("claude", "install")
        self.assertEqual(self.claude.read_bytes(), b"Concurrent human edit")
        self.assertEqual(list(self.claude.parent.glob("*.tmp")), [])

    def test_concurrent_parent_replacement_is_not_overwritten(self):
        self.write(self.claude, b"Original")
        real_write = integration._private_write
        moved = self.home / "old-claude"

        def replace_parent(descriptor, name, data, mode=0o600):
            real_write(descriptor, name, data, mode)
            if name.endswith(".tmp"):
                self.claude.parent.rename(moved)
                self.claude.parent.mkdir(mode=0o700)
                self.claude.write_bytes(b"New directory contents")

        with mock.patch.object(integration, "_private_write", side_effect=replace_parent):
            with self.assertRaisesRegex(integration.IntegrationError, "directory changed"):
                integration.change("claude", "install")
        self.assertEqual(self.claude.read_bytes(), b"New directory contents")
        self.assertEqual((moved / "CLAUDE.md").read_bytes(), b"Original")

    def test_codex_concurrent_override_creation_is_refused(self):
        self.write(self.codex, b"Original")
        real_write = integration._private_write

        def create_override(descriptor, name, data, mode=0o600):
            real_write(descriptor, name, data, mode)
            if name.endswith(".tmp"):
                self.codex.with_name("AGENTS.override.md").write_bytes(b"New higher-priority rules")

        with mock.patch.object(integration, "_private_write", side_effect=create_override):
            with self.assertRaisesRegex(integration.IntegrationError, "precedence"):
                integration.change("codex", "install")
        self.assertEqual(self.codex.read_bytes(), b"Original")

    def test_backup_failure_prevents_replacement(self):
        self.write(self.claude, b"Original")
        with mock.patch.object(integration, "_private_write", side_effect=OSError("backup unavailable")):
            with self.assertRaisesRegex(OSError, "backup unavailable"):
                integration.change("claude", "install")
        self.assertEqual(self.claude.read_bytes(), b"Original")

    def test_cli_json_and_argument_errors(self):
        initial = self.cli("status", "--json")
        self.assertEqual(initial.returncode, 0, initial.stderr)
        self.assertEqual(json.loads(initial.stdout), integration.snapshot())
        self.assertEqual(list(self.home.iterdir()), [])
        installed = self.cli("install", "--runner", "claude", "--json")
        self.assertEqual(installed.returncode, 0, installed.stderr)
        self.assertTrue(next(row for row in json.loads(installed.stdout)["runners"] if row["id"] == "claude")["configured"])
        removed = self.cli("remove", "--runner", "claude", "--json")
        self.assertEqual(removed.returncode, 0, removed.stderr)
        self.assertFalse(self.row("claude")["configured"])
        missing = self.cli("install", "--json")
        self.assertEqual(missing.returncode, 2)
        self.assertIn("--runner is required", missing.stderr)
        invalid = self.cli("install", "--runner", "unknown")
        self.assertEqual(invalid.returncode, 2)

    def test_json_status_and_sibling_errors_remain_inspectable(self):
        os.environ["CODEX_HOME"] = "unsafe-relative-directory"
        status = self.cli("status", "--json")
        self.assertEqual(status.returncode, 0, status.stderr)
        codex = next(row for row in json.loads(status.stdout)["runners"] if row["id"] == "codex")
        self.assertIsNone(codex["path"])
        self.assertIsNotNone(codex["error"])
        self.assertEqual(self.cli("status").returncode, 1)
        installed = self.cli("install", "--runner", "claude", "--json")
        self.assertEqual(installed.returncode, 0, installed.stderr)
        self.assertTrue(next(row for row in json.loads(installed.stdout)["runners"]
                             if row["id"] == "claude")["configured"])
        self.assertEqual(self.cli("install", "--runner", "codex", "--json").returncode, 1)

    def test_queue_cli_delegates_without_creating_queue_or_instruction_state(self):
        result = subprocess.run([sys.executable, str(ROOT / "agent_heavy.py"), "agents", "status", "--json"],
                                env=os.environ.copy(), capture_output=True, text=True, check=False, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), integration.snapshot())
        self.assertEqual(list(self.home.iterdir()), [])
        installed = subprocess.run([sys.executable, str(ROOT / "agent_heavy.py"), "agents", "install",
                                    "--runner", "claude", "--json"], env=os.environ.copy(),
                                   capture_output=True, text=True, check=False, timeout=5)
        self.assertEqual(installed.returncode, 0, installed.stderr)
        self.assertTrue(self.row("claude")["configured"])
        self.assertFalse((self.home / ".local").exists())
        self.assertFalse((self.home / ".config").exists())

    def test_cli_mutation_error_is_actionable_and_does_not_disclose_contents(self):
        secret = b"<!-- agent-job-queue:wrong -->\nPRIVATE CONTENT"
        self.write(self.claude, secret)
        result = self.cli("install", "--runner", "claude", "--json")
        self.assertEqual(result.returncode, 1)
        self.assertIn("edited", result.stderr)
        self.assertNotIn("PRIVATE CONTENT", result.stderr + result.stdout)
        self.assertEqual(self.claude.read_bytes(), secret)
        status = self.cli("status", "--json")
        self.assertEqual(status.returncode, 0)
        self.assertIsNotNone(next(row for row in json.loads(status.stdout)["runners"] if row["id"] == "claude")["error"])


if __name__ == "__main__":
    unittest.main()
