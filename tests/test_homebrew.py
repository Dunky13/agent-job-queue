"""Trusted manifest rendering and real Homebrew installer-artifact probes."""
from __future__ import annotations

import base64
import hashlib
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import test_installer

ROOT = Path(__file__).resolve().parents[1]
PACKAGING = ROOT / "packaging/homebrew"
sys.path.insert(0, str(PACKAGING))


def load(name):
    spec = importlib.util.spec_from_file_location(name, PACKAGING / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


renderer = load("render_cask")
refresh = load("refresh")


class HomebrewTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="agent-queue-homebrew-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.archive = self.root / "Agent-Job-Queue-Installer-0.1.0-universal.zip"
        self.archive.write_bytes(b"local package fixture; never published")
        self.manifest = renderer.create_manifest(self.archive, version="0.1.0", tag="build-123-abcdef0",
                                                 commit="abcdef0" + "1" * 33, build=123)
        self.release = {"tag_name": self.manifest["tag"], "target_commitish": self.manifest["commit"],
                        "assets": [{"name": self.manifest["asset"]}, {"name": "agent-job-queue.json"}]}

    def test_renderer_uses_real_checksum_and_immutable_identity(self):
        cask = renderer.render_cask(self.manifest)
        self.assertIn(hashlib.sha256(self.archive.read_bytes()).hexdigest(), cask)
        self.assertIn('version "0.1.0,123"', cask)
        self.assertIn("/releases/download/build-123-abcdef0/", cask)
        self.assertNotIn(":no_check", cask)
        self.assertNotIn("installer manual:", cask)
        self.assertNotIn("zap ", cask)
        self.assertIn('depends_on formula: "pueue"', cask)
        self.assertIn('binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/bin/agent-heavy"', cask)

    def test_manifest_rejects_untrusted_fields_and_code_or_path_injection(self):
        invalid = [("repository", "Other/project"), ("url", "https://untrusted.example/package"),
                   ("version", '0.1.0"; system("bad")'), ("build", True), ("build", -1),
                   ("commit", "main"), ("sha256", "0" * 63), ("asset", "../installer.zip"),
                   ("tag", "build-124-abcdef0"), ("schemaVersion", True)]
        for field, value in invalid:
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                renderer.render_cask({**self.manifest, field: value})

    def test_release_selection_is_monotonic_and_ignores_drafts_or_missing_metadata(self):
        older = {**self.release, "tag_name": "build-12-abcdef0"}
        draft = {**self.release, "tag_name": "build-999-abcdef0", "draft": True}
        incomplete = {**self.release, "tag_name": "build-888-abcdef0", "assets": []}
        malformed = {**self.release, "tag_name": "bad; shell"}
        self.assertEqual(refresh.select_release([older, draft, self.release, incomplete, malformed]), self.release)
        self.assertIsNone(refresh.select_release([]))

    def test_release_verification_checks_commit_asset_and_actual_download(self):
        self.assertEqual(refresh.verify_release(self.manifest, self.release, self.archive), self.manifest)
        for release in ({**self.release, "target_commitish": "main"},
                        {**self.release, "tag_name": "build-124-abcdef0"}, {**self.release, "assets": []}):
            with self.subTest(release=release), self.assertRaises(ValueError):
                refresh.verify_release(self.manifest, release, self.archive)
        self.archive.write_bytes(b"tampered download")
        with self.assertRaisesRegex(ValueError, "checksum"):
            refresh.verify_release(self.manifest, self.release, self.archive)

    def test_publication_pins_head_and_verifies_signature_and_matching_dco(self):
        verified = {"commit": {"author": {"name": refresh.BOT_NAME, "email": refresh.BOT_EMAIL},
                               "verification": {"verified": True},
                               "message": f"chore(cask): publish\n\nSigned-off-by: {refresh.BOT_NAME} <{refresh.BOT_EMAIL}>"}}
        responses = [{"viewer": {"login": refresh.BOT_NAME}}, {"createCommitOnBranch": {"commit": {"oid": "1" * 40}}}]
        with patch.object(refresh, "graphql", side_effect=responses) as calls, patch.object(refresh, "api", return_value=verified):
            refresh.publish(self.manifest, renderer.render_cask(self.manifest), "2" * 40)
        payload = calls.call_args_list[1].args[1]["input"]
        self.assertEqual(payload["expectedHeadOid"], "2" * 40)
        self.assertEqual(payload["branch"]["branchName"], "tap")
        additions = {item["path"]: base64.b64decode(item["contents"]).decode()
                     for item in payload["fileChanges"]["additions"]}
        self.assertEqual(additions["Casks/agent-job-queue.rb"], renderer.render_cask(self.manifest))
        self.assertIn("Signed-off-by:", payload["message"]["body"])
        with patch.object(refresh, "graphql", return_value={"viewer": {"login": "ordinary-user"}}), self.assertRaises(ValueError):
            refresh.publish(self.manifest, "", "2" * 40)

    def test_remote_head_advanced_since_checkout_cannot_publish_or_downgrade(self):
        with patch.object(refresh, "command", return_value="1" * 40 + "\n"), \
                patch.object(refresh, "api", return_value={"object": {"sha": "2" * 40}}) as api, \
                patch.object(refresh, "publish") as publish:
            with self.assertRaisesRegex(RuntimeError, "checkout is stale"):
                refresh.refresh(publish_changes=True)
            api.assert_called_once()
            publish.assert_not_called()

    def test_real_homebrew_dsl_runs_automated_install_in_private_fixture(self):
        brew = shutil.which("brew", path=os.environ.get("PATH", "") + ":/opt/homebrew/bin:/usr/local/bin")
        self.assertIsNotNone(brew, "Homebrew packaging checks require Homebrew on macOS")
        fixture = test_installer.InstallerTests(methodName="runTest")
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        keys = ("HOME", "AGENT_QUEUE_INSTALL_TEST_MODE", "AGENT_QUEUE_INSTALL_TEST_TOOLS",
                "TEST_PYTHON_PREFIX", "TEST_CALLS", "TEST_PYTHON")
        fixture_environment = fixture.base / "fixture-environment.json"
        fixture_environment.write_text(json.dumps({key: fixture.environment[key] for key in keys}))
        fixture_environment.chmod(0o600)
        cask = self.root / "agent-job-queue.rb"
        cask.write_text(renderer.render_cask(self.manifest))
        environment = {**fixture.environment, "HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_DEVELOPER": "1"}
        result = subprocess.run([brew, "ruby", str(PACKAGING / "test-cask.rb"), str(cask), str(fixture.base)],
                                env=environment, capture_output=True, text=True, timeout=45, check=False)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue((fixture.home / "Applications/Agent Queue.app/Contents/MacOS/AgentQueue").exists())
        self.assertTrue((fixture.home / ".local/bin/agent-heavy").exists())
        self.assertTrue((fixture.home / "Library/LaunchAgents/io.went.agent-heavy.plist").exists())
        self.assertFalse((fixture.home / ".config/agent-heavy/remote.json").exists())
        data = json.loads(result.stdout.splitlines()[-1])
        self.assertEqual(data["version"], "0.1.0,123")
        self.assertEqual(data["sha256"], self.manifest["sha256"])
        self.assertIn("Installed Agent Queue", result.stdout)


if __name__ == "__main__":
    unittest.main()
