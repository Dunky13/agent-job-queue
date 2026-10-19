"""Installer isolation and upgrade guarantees; no real host services are touched."""
from __future__ import annotations

import json
import os
import plistlib
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="agent-queue-install-test-")
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.home = self.base / "User With Spaces"
        self.home.mkdir()
        self.payload = self.base / "payload"
        self.source = self.payload / "source"
        self.source.mkdir(parents=True)
        for name in ("agent_heavy.py", "install-remote.sh"):
            shutil.copy2(ROOT / name, self.source / name)
        shutil.copytree(ROOT / "bin", self.source / "bin")
        shutil.copytree(ROOT / "remote", self.source / "remote", ignore=shutil.ignore_patterns(".venv", "__pycache__"))
        (self.source / "scripts").mkdir()
        shutil.copy2(ROOT / "scripts/install-user.sh", self.source / "scripts/install-user.sh")
        shutil.copy2(ROOT / "scripts/migrate-installation.py", self.source / "scripts/migrate-installation.py")
        self.app = self.payload / "Agent Queue.app/Contents/MacOS"
        self.app.mkdir(parents=True)
        (self.app / "AgentQueue").write_text("prebuilt app")
        self.tools = self.base / "tools"
        self.tools.mkdir()
        self.python_prefix = self.base / "python"
        (self.python_prefix / "bin").mkdir(parents=True)
        (self.python_prefix / "bin/python3.12").symlink_to(sys.executable)
        self.calls = self.base / "calls"
        self.environment = {
            **{key: value for key, value in os.environ.items() if not key.startswith(("AGENT_HEAVY_", "PUEUE_"))},
            "HOME": str(self.home),
            "AGENT_QUEUE_INSTALL_TEST_MODE": "1",
            "AGENT_QUEUE_INSTALL_TEST_TOOLS": str(self.tools),
            "TEST_PYTHON_PREFIX": str(self.python_prefix),
            "TEST_CALLS": str(self.calls),
            "TEST_PYTHON": sys.executable,
        }
        self.tool("brew", 'if [ "$1" = --prefix ]; then printf "%s\\n" "$TEST_PYTHON_PREFIX"; fi')
        self.tool("uv", '''if [ "$1" = venv ]; then
  for last; do :; done
  mkdir -p "$last/bin"
  ln -s "$TEST_PYTHON" "$last/bin/python"
fi''')
        self.tool("pueue", "exit 0")
        self.tool("pueued", "exit 0")
        self.tool("caddy", "exit 0")
        self.tool("launchctl", 'if [ "$1" = print ]; then exit "${TEST_LOADED:-1}"; fi')

    def tool(self, name, body):
        file = self.tools / name
        file.write_text('#!/bin/bash\nprintf "%s" "$(basename "$0")" >> "$TEST_CALLS"\nprintf " <%s>" "$@" >> "$TEST_CALLS"\nprintf "\\n" >> "$TEST_CALLS"\n' + body + "\n")
        file.chmod(0o755)

    def install(self):
        return subprocess.run(["/bin/bash", str(ROOT / "scripts/install-user.sh"), "--payload", str(self.payload)],
                              env=self.environment, capture_output=True, text=True, timeout=30, check=False)

    def test_fresh_install_creates_private_dedicated_queue_and_no_remote_credentials(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        config = self.home / ".config/agent-heavy/pueue.yml"
        data = json.loads(config.read_text())
        self.assertTrue(data["shared"]["use_unix_socket"])
        self.assertEqual(data["shared"]["unix_socket_permissions"], 0o600)
        self.assertIn(str(self.home), data["daemon"]["callback"])
        self.assertEqual(config.stat().st_mode & 0o777, 0o600)
        launch = self.home / "Library/LaunchAgents/io.went.agent-heavy.plist"
        service = plistlib.loads(launch.read_bytes())
        self.assertEqual(service["ProgramArguments"][0], str(self.tools / "pueued"))
        self.assertIn("/usr/local/bin", service["EnvironmentVariables"]["PATH"])
        self.assertTrue((self.home / "Applications/Agent Queue.app/Contents/MacOS/AgentQueue").exists())
        self.assertTrue((self.home / ".local/bin/agent-heavy").stat().st_mode & 0o100)
        self.assertFalse((self.home / ".config/agent-heavy/remote.json").exists())
        calls = self.calls.read_text()
        self.assertIn("launchctl <bootstrap>", calls)
        self.assertIn("<parallel> <1>", calls)
        self.assertNotIn("brew <install>", calls)
        self.assertIn("<--require-hashes>", calls)
        self.assertIn("remote/requirements.lock", calls)
        self.assertNotIn("<kill>", calls)

    def test_upgrade_preserves_configuration_state_and_running_daemon(self):
        self.assertEqual(self.install().returncode, 0)
        config = self.home / ".config/agent-heavy/pueue.yml"
        config.write_text('{"existing": "operator configuration"}')
        launch = self.home / "Library/LaunchAgents/io.went.agent-heavy.plist"
        old_launch = launch.read_bytes()
        state = self.home / ".local/state/agent-heavy/pueue/state.json"
        state.write_text('{"active": [123], "limit": 3}')
        remote = self.home / ".config/agent-heavy/remote.json"
        remote.write_text("private credentials remain untouched")
        self.calls.write_text("")
        self.environment["TEST_LOADED"] = "0"
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(config.read_text(), '{"existing": "operator configuration"}')
        self.assertEqual(state.read_text(), '{"active": [123], "limit": 3}')
        self.assertEqual(launch.read_bytes(), old_launch)
        self.assertEqual(remote.read_text(), "private credentials remain untouched")
        calls = self.calls.read_text()
        self.assertNotIn("<bootstrap>", calls)
        self.assertNotIn("<bootout>", calls)
        self.assertNotIn("<parallel>", calls)
        self.assertNotIn("<kill>", calls)

    def test_explicit_operator_overrides_survive_daemon_environment(self):
        self.environment["AGENT_HEAVY_PUEUE"] = str(self.tools / "pueue")
        self.environment["AGENT_HEAVY_LEGACY_SLOT"] = str(self.base / "legacy bridge.slot")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        launch = self.home / "Library/LaunchAgents/io.went.agent-heavy.plist"
        environment = plistlib.loads(launch.read_bytes())["EnvironmentVariables"]
        self.assertEqual(environment["AGENT_HEAVY_PUEUE"], str(self.tools / "pueue"))
        self.assertEqual(environment["AGENT_HEAVY_LEGACY_SLOT"], str(self.base / "legacy bridge.slot"))

    def test_installed_wrappers_handle_spaces_and_preserve_arguments_and_exit_status(self):
        self.assertEqual(self.install().returncode, 0)
        support = self.home / ".local/share/agent-heavy"
        for name, module in (("agent-heavy", support / "agent_heavy.py"),
                             ("agent-remote", support / "remote/control.py")):
            module.write_text("import sys\nprint(repr(sys.argv[1:]))\nsys.exit(7)\n")
            result = subprocess.run([str(self.home / ".local/bin" / name), "argument with spaces", "literal;$HOME"],
                                    env=self.environment, capture_output=True, text=True, timeout=5, check=False)
            self.assertEqual(result.returncode, 7)
            self.assertEqual(result.stdout.strip(), "['argument with spaces', 'literal;$HOME']")

    def test_legacy_source_is_migrated_without_executing_it(self):
        support = self.home / ".local/share/agent-heavy"
        support.mkdir(parents=True)
        legacy = self.base / "older queue.slot"
        marker = self.base / "must not execute"
        old = "from pathlib import Path\nraise RuntimeError('must not execute')\nclass Gate:\n    def __init__(self):\n        self.slot = Path(os.environ.get(OVERRIDES[2], " + repr(str(legacy)) + "))\n"
        (support / "agent_heavy.py").write_text(old)
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        migration = self.home / ".local/state/agent-heavy/legacy-slot.json"
        self.assertEqual(json.loads(migration.read_text()), {"slot": str(legacy)})
        self.assertEqual(migration.stat().st_mode & 0o777, 0o600)
        self.assertFalse(marker.exists())
        self.assertEqual(self.install().returncode, 0)
        self.assertEqual(json.loads(migration.read_text()), {"slot": str(legacy)})

    def test_unknown_installed_source_stops_before_replacement(self):
        support = self.home / ".local/share/agent-heavy"
        support.mkdir(parents=True)
        original = "class Gate: pass\n"
        backend = support / "agent_heavy.py"
        backend.write_text(original)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Upgrade stopped before replacing installed source", result.stderr)
        self.assertEqual(backend.read_text(), original)

    def test_real_daemon_with_installer_config_and_wrappers(self):
        search = os.environ.get("PATH", "") + ":/opt/homebrew/bin:/usr/local/bin"
        client = shutil.which("pueue", path=search)
        daemon_tool = shutil.which("pueued", path=search)
        self.assertIsNotNone(client, "Installer smoke requires installed pueue")
        self.assertIsNotNone(daemon_tool, "Installer smoke requires installed pueued")
        # Use a short HOME so the unmodified installer-generated Unix socket fits macOS limits.
        short = tempfile.TemporaryDirectory(prefix="aq-s-", dir="/tmp")
        self.addCleanup(short.cleanup)
        self.home = Path(short.name) / "home"
        self.home.mkdir()
        self.environment["HOME"] = str(self.home)
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        config = self.home / ".config/agent-heavy/pueue.yml"
        state = self.home / ".local/state/agent-heavy"
        environment = {**self.environment, "AGENT_HEAVY_PUEUE": client,
                       "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
        log = self.base / "real-daemon.log"
        with log.open("w+") as output:
            daemon = subprocess.Popen([daemon_tool, "--config", str(config)], env=environment,
                                      stdout=output, stderr=output)
            try:
                deadline = time.monotonic() + 12
                while time.monotonic() < deadline:
                    ready = subprocess.run([client, "--config", str(config), "status", "--json"],
                                           env=environment, capture_output=True, timeout=2, check=False)
                    if ready.returncode == 0:
                        break
                    self.assertIsNone(daemon.poll(), log.read_text())
                    time.sleep(0.05)
                else:
                    self.fail("Installer-produced Pueue config did not become ready: " + log.read_text())
                wrapper = self.home / ".local/bin/agent-heavy"
                job = subprocess.run([str(wrapper), "run", "--label", "Installer smoke", "--",
                                      sys.executable, "-c", "print('installer smoke output'); raise SystemExit(7)"],
                                     env=environment, capture_output=True, text=True, timeout=20, check=False)
                self.assertEqual(job.returncode, 7, job.stdout + job.stderr + log.read_text())
                self.assertEqual(job.stdout, "installer smoke output\n")
                deadline = time.monotonic() + 10
                while ((state / "execution.slot").exists() or list((state / "receipts").glob("*.json"))) \
                        and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertFalse((state / "execution.slot").exists(), log.read_text())
                self.assertEqual(list((state / "receipts").glob("*.json")), [])
                self.assertEqual(list((state / "jobs").glob("*.json")), [])
                status = subprocess.run([str(wrapper), "status", "--json"], env=environment,
                                        capture_output=True, text=True, timeout=5, check=True)
                jobs = json.loads(status.stdout)["jobs"]
                self.assertEqual(len(jobs), 1)
                self.assertEqual(jobs[0]["exitCode"], 7)
            finally:
                if daemon.poll() is None:
                    daemon.terminate()
                    try:
                        daemon.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        daemon.kill()
                        daemon.wait(timeout=5)

    def test_concurrent_install_is_rejected_without_changing_queue(self):
        lock = self.home / ".local/state/agent-heavy/install.lock"
        lock.parent.mkdir(parents=True)
        holder = subprocess.Popen(["/usr/bin/lockf", "-k", "-t", "0", str(lock), sys.executable,
                                   "-c", "import sys; print('locked', flush=True); sys.stdin.read()"],
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.assertEqual(holder.stdout.readline().strip(), "locked")
        self.addCleanup(holder.communicate, "", 5)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Another installer", result.stderr)
        self.assertFalse((self.home / ".config/agent-heavy/pueue.yml").exists())

    def test_stale_install_lock_is_recovered_after_process_death(self):
        lock = self.home / ".local/state/agent-heavy/install.lock"
        lock.parent.mkdir(parents=True)
        lock.write_text("99999999\n")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(lock.is_file())

    def test_incomplete_payload_fails_before_installing(self):
        (self.source / "agent_heavy.py").unlink()
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Incomplete installer", result.stderr)
        self.assertFalse(self.calls.exists())

    def test_installer_failure_does_not_replace_existing_app(self):
        self.tool("uv", "exit 42")
        installed = self.home / "Applications/Agent Queue.app/Contents/MacOS"
        installed.mkdir(parents=True)
        existing = installed / "AgentQueue"
        existing.write_text("existing executable")
        result = self.install()
        self.assertEqual(result.returncode, 42)
        self.assertEqual(existing.read_text(), "existing executable")
        self.assertTrue((self.home / ".local/state/agent-heavy/install.lock").is_file())


if __name__ == "__main__":
    unittest.main()
