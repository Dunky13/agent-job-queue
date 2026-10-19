"""Build identity provenance and the shared release trust boundary."""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from release_metadata import REPOSITORY, generate_build_info, validate_build_info, validate_manifest

ROOT = Path(__file__).resolve().parents[1]
COMMIT = "abcdef0" + "1" * 33


class BuildMetadataTests(unittest.TestCase):
    def test_local_checkout_and_pull_requests_have_no_release_build(self):
        contexts = ({}, {"GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": COMMIT},
                    {"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": REPOSITORY,
                     "GITHUB_EVENT_NAME": "pull_request", "GITHUB_REF": "refs/pull/7/merge",
                     "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": COMMIT},
                    {"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "Other/project",
                     "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
                     "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": COMMIT})
        for environment in contexts:
            with self.subTest(environment=environment):
                identity = generate_build_info("0.1.0", environment=environment)
                self.assertIsNone(identity["build"])
                self.assertIsNone(identity["commit"])

    def test_published_main_pipeline_has_exact_immutable_identity(self):
        identity = generate_build_info("0.1.0", environment={
            "GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": REPOSITORY,
            "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
            "GITHUB_RUN_NUMBER": "123", "GITHUB_SHA": COMMIT,
        })
        self.assertEqual(identity, {"schemaVersion": 1, "repository": REPOSITORY,
                                    "version": "0.1.0", "build": 123, "commit": COMMIT})

    def test_explicit_build_pair_is_validated_and_overrides_ci(self):
        identity = generate_build_info("1.2.3-rc.1+package.4", environment={
            "BUILD_NUMBER": "456", "SOURCE_COMMIT": COMMIT,
            "GITHUB_RUN_NUMBER": "999", "GITHUB_SHA": "2" * 40,
        })
        self.assertEqual(identity["version"], "1.2.3-rc.1+package.4")
        self.assertEqual(identity["build"], 456)
        self.assertEqual(identity["commit"], COMMIT)
        for environment in ({"BUILD_NUMBER": "1"}, {"SOURCE_COMMIT": COMMIT},
                            {"BUILD_NUMBER": "", "SOURCE_COMMIT": COMMIT},
                            {"BUILD_NUMBER": "01", "SOURCE_COMMIT": COMMIT},
                            {"BUILD_NUMBER": "1000000000", "SOURCE_COMMIT": COMMIT},
                            {"BUILD_NUMBER": "1", "SOURCE_COMMIT": "main"}):
            with self.subTest(environment=environment), self.assertRaises(ValueError):
                generate_build_info("0.1.0", environment=environment)

    def test_incomplete_published_pipeline_fails_instead_of_claiming_local(self):
        with self.assertRaisesRegex(ValueError, "incomplete"):
            generate_build_info("0.1.0", environment={
                "GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": REPOSITORY,
                "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/heads/main",
                "GITHUB_RUN_NUMBER": "123",
            })

    def test_build_validator_rejects_unknown_source_fields_and_bad_types(self):
        valid = generate_build_info("0.1.0", environment={"BUILD_NUMBER": "123", "SOURCE_COMMIT": COMMIT})
        invalid = [("schemaVersion", True), ("schemaVersion", 2), ("repository", "Other/project"),
                   ("build", True), ("build", 0), ("build", -1), ("build", 1.0),
                   ("commit", None), ("commit", "0" * 39), ("commit", "A" * 40),
                   ("version", "01.2.3"), ("version", "1.2"), ("version", "1.2.3.4"),
                   ("version", "1.2.3-01"), ("version", "1.2.3-rc..1"),
                   ("version", "1.2.3\n"), ("version", '1.2.3";exit'),
                   ("url", "https://other.example/installer")]
        for field, value in invalid:
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                validate_build_info({**valid, field: value})
        with self.assertRaises(ValueError):
            validate_build_info({key: value for key, value in valid.items() if key != "commit"})

    def test_release_requires_tracked_build_and_pinned_tag_asset_and_checksum(self):
        identity = generate_build_info("0.1.0", environment={"BUILD_NUMBER": "123", "SOURCE_COMMIT": COMMIT})
        manifest = {**identity, "tag": "build-123-abcdef0", "sha256": "1" * 64,
                    "asset": "Agent-Job-Queue-Installer-0.1.0-universal.zip"}
        self.assertEqual(validate_manifest(manifest), manifest)
        for change in ({"build": None}, {"sha256": "0" * 63}, {"tag": "build-124-abcdef0"},
                       {"asset": "../installer.zip"}, {"unexpected": "value"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                validate_manifest({**manifest, **change})

    def test_command_writes_valid_identity_from_documented_pair(self):
        with tempfile.TemporaryDirectory(prefix="agent-queue-build-info-") as temporary:
            destination = Path(temporary) / "build-info.json"
            environment = {**os.environ, "BUILD_NUMBER": "123", "SOURCE_COMMIT": COMMIT}
            result = subprocess.run([sys.executable, str(ROOT / "release_metadata.py"), "--version", "0.1.0",
                                     "--output", str(destination)], env=environment,
                                    capture_output=True, text=True, timeout=5, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(validate_build_info(json.loads(destination.read_text()))["build"], 123)


if __name__ == "__main__":
    unittest.main()
