"""Source-to-tap delivery uses an exact run and independently verified pinned files."""
from __future__ import annotations

import base64
import copy
import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

PACKAGING = Path(__file__).resolve().parents[1] / "packaging/homebrew"
sys.path.insert(0, str(PACKAGING))
SPEC = importlib.util.spec_from_file_location("homebrew_sync", PACKAGING / "sync.py")
sync = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(sync)

SOURCE = f"repos/{sync.REPOSITORY}"
TAP = f"repos/{sync.TAP_REPOSITORY}"
HEAD = "1" * 40
NEXT_HEAD = "2" * 40
RUN = 987
DISPATCH = f"{TAP}/actions/workflows/refresh.yml/dispatches"
RUN_ENDPOINT = f"{TAP}/actions/runs/{RUN}"
REF_ENDPOINT = f"{TAP}/git/ref/heads/tap"
ARCHIVE = b"fixture installer archive; never installed"


def manifest(build=123, commit="a" * 40):
    return {"schemaVersion": 1, "repository": sync.REPOSITORY, "tag": f"build-{build}-{commit[:7]}",
            "commit": commit, "version": "0.1.0", "build": build,
            "asset": "Agent-Job-Queue-Installer-0.1.0-universal.zip", "sha256": hashlib.sha256(ARCHIVE).hexdigest()}


def content(path, body):
    return {"type": "file", "path": path, "encoding": "base64", "size": len(body),
            "content": base64.b64encode(body).decode()}


def verified_commit(sha=HEAD):
    return {"sha": sha, "commit": {"author": {"name": "Fixture Bot", "email": "fixture@example.invalid"},
                                   "verification": {"verified": True},
                                   "message": "chore(cask): publish\n\n"
                                              "Signed-off-by: Fixture Bot <fixture@example.invalid>"}}


class FakeGitHub:
    def __init__(self):
        self.responses = {}
        self.calls = []
        self.deadline = Mock()

    def value(self, endpoint, options):
        if not endpoint.startswith((SOURCE + "/", TAP + "/")):
            raise AssertionError("API request escaped the two fixed repositories")
        self.calls.append((endpoint, options))
        response = self.responses[endpoint]
        if callable(response):
            response = response()
        return copy.deepcopy(response)

    def json(self, endpoint, **options):
        return self.value(endpoint, options)

    def request(self, endpoint, **options):
        return self.value(endpoint, options)

    def release(self, data):
        body = json.dumps(data).encode()
        identifier = data["build"] * 10
        self.responses[f"{SOURCE}/releases/tags/{data['tag']}"] = {
            "draft": False, "tag_name": data["tag"], "target_commitish": data["commit"],
            "published_at": "2026-10-19T12:00:00Z", "assets": [
                {"name": "agent-job-queue.json", "id": identifier, "size": len(body), "state": "uploaded",
                 "digest": f"sha256:{hashlib.sha256(body).hexdigest()}"},
                {"name": data["asset"], "id": identifier + 1, "size": len(ARCHIVE), "state": "uploaded",
                 "digest": f"sha256:{hashlib.sha256(ARCHIVE).hexdigest()}"}]}
        self.responses[f"{SOURCE}/releases/assets/{identifier}"] = body

    def file(self, path, body, sha=HEAD):
        endpoint = f"{TAP}/contents/{path}?ref={sha}"
        self.responses[endpoint] = content(path, body)
        return endpoint

    def tap(self, data, sha=HEAD):
        self.responses[REF_ENDPOINT] = {"ref": "refs/heads/tap", "object": {"sha": sha, "type": "commit"}}
        self.responses[f"{TAP}/commits/{sha}"] = verified_commit(sha)
        self.file("Casks/agent-job-queue.json", json.dumps(data).encode(), sha)
        self.file("Casks/agent-job-queue.rb", sync.render_cask(data).encode(), sha)
        for name, path in sync.TOOLING.items():
            self.file(name, path.read_bytes(), sha)


class HomebrewSyncTests(unittest.TestCase):
    def setUp(self):
        self.expected = manifest()
        self.github = FakeGitHub()
        self.github.release(self.expected)
        self.github.tap(self.expected)
        self.github.responses[DISPATCH] = {"workflow_run_id": RUN, "html_url": sync.run_url(RUN),
                                           "run_url": f"https://api.github.com/{RUN_ENDPOINT}"}
        self.github.responses[RUN_ENDPOINT] = {
            "id": RUN, "repository": {"full_name": sync.TAP_REPOSITORY}, "head_branch": "tap",
            "event": "workflow_dispatch", "path": ".github/workflows/refresh.yml", "status": "completed",
            "conclusion": "success"}

    def synchronize(self):
        with redirect_stdout(io.StringIO()) as output:
            result = sync.synchronize(self.expected, self.github, poll_interval=2)
        return result, output.getvalue()

    def test_exact_dispatch_and_readback_verify_current_build(self):
        result, output = self.synchronize()
        self.assertEqual(result, {"status": "current", "build": 123, "commit": HEAD})
        self.assertIn("Dispatched tap refresh for build 123", output)
        self.assertIn("Verified Homebrew current at build 123", output)
        dispatched = [options for endpoint, options in self.github.calls if endpoint == DISPATCH]
        self.assertEqual(dispatched, [{"actions": True, "data": {"ref": "tap", "inputs": {
            "expected_tag": self.expected["tag"], "expected_commit": self.expected["commit"]}}}])
        for endpoint, options in self.github.calls:
            self.assertEqual(options.get("actions", False), endpoint in {DISPATCH, RUN_ENDPOINT})
        pinned = [endpoint for endpoint, _ in self.github.calls if "/contents/" in endpoint]
        self.assertTrue(pinned)
        self.assertTrue(all(endpoint.endswith(f"?ref={HEAD}") for endpoint in pinned))

    def test_invalid_or_foreign_manifest_cannot_dispatch(self):
        for invalid in ({**self.expected, "repository": "other/project"}, {**self.expected, "build": True},
                        {**self.expected, "tag": "../../actions/runs/1"}):
            with self.subTest(invalid=invalid), self.assertRaises(sync.SyncError):
                sync.synchronize(invalid, self.github, poll_interval=1)
        self.assertEqual(self.github.calls, [])

    def test_dispatch_requires_positive_integer_exact_run_and_known_urls(self):
        original = self.github.responses[DISPATCH]
        invalid = [{}, *[{**original, "workflow_run_id": value} for value in (None, True, "987", 0, -1, 2**64)],
                   {**original, "html_url": "https://untrusted.invalid/run"},
                   {**original, "run_url": "https://api.github.com/repos/other/project/actions/runs/987"}]
        for response in invalid:
            with self.subTest(response=response), self.assertRaises(sync.SyncError):
                self.github.responses[DISPATCH] = response
                sync.dispatch(self.expected, self.github)

    def test_wait_polls_only_returned_run_through_queued_and_running_states(self):
        original = self.github.responses[RUN_ENDPOINT]
        responses = iter([{**original, "status": "queued", "conclusion": None},
                          {**original, "status": "in_progress", "conclusion": None}, original])
        self.github.responses[RUN_ENDPOINT] = lambda: next(responses)
        sync.wait_for_run(RUN, self.github, 2)
        self.assertEqual([endpoint for endpoint, _ in self.github.calls], [RUN_ENDPOINT] * 3)
        self.assertEqual(self.github.deadline.pause.call_count, 2)

    def test_every_terminal_non_success_fails_visibly(self):
        for conclusion in ("failure", "cancelled", "timed_out", "skipped", "neutral", "action_required", None):
            with self.subTest(conclusion=conclusion), self.assertRaisesRegex(sync.SyncError, sync.run_url(RUN)):
                self.github.responses[RUN_ENDPOINT]["conclusion"] = conclusion
                sync.wait_for_run(RUN, self.github, 2)

    def test_run_identity_cannot_be_substituted_or_malformed(self):
        original = self.github.responses[RUN_ENDPOINT]
        for field, value in (("id", RUN + 1), ("repository", None),
                             ("repository", {"full_name": "other/project"}), ("head_branch", "main"),
                             ("event", "push"), ("path", ".github/workflows/other.yml")):
            with self.subTest(field=field), self.assertRaisesRegex(sync.SyncError, "unexpected"):
                self.github.responses[RUN_ENDPOINT] = {**original, field: value}
                sync.wait_for_run(RUN, self.github, 2)

    def test_unknown_run_state_and_deadline_are_failures(self):
        for status in ("surprising", ["queued"], None):
            self.github.responses[RUN_ENDPOINT]["status"] = status
            with self.subTest(status=status), self.assertRaisesRegex(sync.SyncError, "unknown"):
                sync.wait_for_run(RUN, self.github, 2)
        self.github.responses[RUN_ENDPOINT]["status"] = "queued"
        self.github.deadline.pause.side_effect = sync.SyncError("deadline")
        with self.assertRaisesRegex(sync.SyncError, "deadline"):
            sync.wait_for_run(RUN, self.github, 2)

    def test_release_identity_assets_and_public_manifest_are_verified_before_dispatch(self):
        endpoint = f"{SOURCE}/releases/tags/{self.expected['tag']}"
        original = self.github.responses[endpoint]
        cases = [{**original, "draft": True}, {**original, "draft": 0},
                 {**original, "target_commitish": "main"}, {**original, "published_at": None},
                 {**original, "tag_name": "build-1-aaaaaaa"}, {**original, "assets": []},
                 {**original, "assets": original["assets"] * 2}]
        for release in cases:
            with self.subTest(release=release), self.assertRaises(sync.SyncError):
                self.github.responses[endpoint] = release
                self.synchronize()
        self.assertNotIn(DISPATCH, [endpoint for endpoint, _ in self.github.calls])

    def test_release_asset_ids_sizes_and_uploaded_state_are_strict(self):
        endpoint = f"{SOURCE}/releases/tags/{self.expected['tag']}"
        original = copy.deepcopy(self.github.responses[endpoint])
        for field, value in (("id", True), ("id", "1230"), ("size", True), ("size", 0),
                             ("size", sync.MAX_METADATA + 1), ("state", "new")):
            release = copy.deepcopy(original)
            release["assets"][0][field] = value
            self.github.responses[endpoint] = release
            with self.subTest(field=field, value=value), self.assertRaises(sync.SyncError):
                sync.verify_release(self.expected, self.github)

    def test_published_manifest_must_match_every_field_and_reported_size(self):
        endpoint = f"{SOURCE}/releases/assets/1230"
        for body in (json.dumps({**self.expected, "sha256": "d" * 64}).encode(), b"{}", b"not JSON"):
            with self.subTest(body=body), self.assertRaises(sync.SyncError):
                self.github.responses[endpoint] = body
                sync.verify_release(self.expected, self.github)

    def test_archive_asset_digest_must_match_manifest_and_reject_replacement(self):
        endpoint = f"{SOURCE}/releases/tags/{self.expected['tag']}"
        original = copy.deepcopy(self.github.responses[endpoint])
        for digest in (None, "", "sha256:invalid", "SHA256:" + self.expected["sha256"],
                       "sha256:" + hashlib.sha256(b"replaced archive").hexdigest()):
            release = copy.deepcopy(original)
            release["assets"][1]["digest"] = digest
            self.github.responses[endpoint] = release
            with self.subTest(digest=digest), self.assertRaisesRegex(sync.SyncError, "installer asset checksum"):
                sync.verify_release(self.expected, self.github)
        del self.github.responses[endpoint]["assets"][1]["digest"]
        with self.assertRaisesRegex(sync.SyncError, "installer asset checksum"):
            sync.verify_release(self.expected, self.github)

    def test_manifest_asset_digest_is_required_and_matches_downloaded_bytes(self):
        endpoint = f"{SOURCE}/releases/tags/{self.expected['tag']}"
        original = copy.deepcopy(self.github.responses[endpoint])
        for digest in (None, "", "sha256:invalid", "sha256:" + "0" * 64):
            release = copy.deepcopy(original)
            release["assets"][0]["digest"] = digest
            self.github.responses[endpoint] = release
            with self.subTest(digest=digest), self.assertRaisesRegex(sync.SyncError, "manifest checksum"):
                sync.verify_release(self.expected, self.github)
        del self.github.responses[endpoint]["assets"][0]["digest"]
        with self.assertRaisesRegex(sync.SyncError, "manifest checksum"):
            sync.verify_release(self.expected, self.github)

    def test_manifest_asset_digest_hashes_raw_bytes_before_json_normalization(self):
        endpoint = f"{SOURCE}/releases/tags/{self.expected['tag']}"
        body = json.dumps(self.expected, indent=2).encode() + b"\n"
        asset = self.github.responses[endpoint]["assets"][0]
        asset["size"] = len(body)
        asset["digest"] = f"sha256:{hashlib.sha256(body).hexdigest()}"
        self.github.responses[f"{SOURCE}/releases/assets/1230"] = body
        sync.verify_release(self.expected, self.github)
        self.github.responses[f"{SOURCE}/releases/assets/1230"] = body + b"\n"
        asset["size"] += 1
        with self.assertRaisesRegex(sync.SyncError, "manifest checksum"):
            sync.verify_release(self.expected, self.github)

    def test_older_or_conflicting_same_number_tap_build_fails(self):
        for data in (manifest(122), {**self.expected, "sha256": "d" * 64},
                     {**self.expected, "version": "0.2.0", "asset": "Agent-Job-Queue-Installer-0.2.0-universal.zip"}):
            self.github.tap(data)
            with self.subTest(data=data), self.assertRaises(sync.SyncError):
                sync.verify_tap(self.expected, self.github)

    def test_tampered_ruby_fails_without_executing_or_trusting_it(self):
        self.github.file("Casks/agent-job-queue.rb", sync.render_cask(self.expected).encode() + b"# tampered")
        with self.assertRaisesRegex(sync.SyncError, "trusted source renderer"):
            sync.verify_tap(self.expected, self.github)

    def test_signature_and_author_matching_dco_are_both_required(self):
        endpoint = f"{TAP}/commits/{HEAD}"
        original = self.github.responses[endpoint]
        mutations = [("verification", {"verified": False}), ("verification", {"verified": 1}),
                     ("message", "Signed-off-by: Somebody Else <fixture@example.invalid>"),
                     ("author", {"name": "Fixture Bot\nother", "email": "fixture@example.invalid"})]
        for field, value in mutations:
            self.github.responses[endpoint] = {**original, "commit": {**original["commit"], field: value}}
            with self.subTest(field=field), self.assertRaises(sync.SyncError):
                sync.verify_tap(self.expected, self.github)

    def test_pinned_commit_and_branch_reference_are_strict(self):
        original = self.github.responses[REF_ENDPOINT]
        for response in (None, {}, {**original, "ref": "refs/heads/main"},
                         {**original, "object": {"sha": "main", "type": "commit"}},
                         {**original, "object": {"sha": HEAD, "type": "tag"}}):
            self.github.responses[REF_ENDPOINT] = response
            with self.subTest(response=response), self.assertRaises(sync.SyncError):
                sync.tap_head(self.github)
        self.github.responses[f"{TAP}/commits/{HEAD}"]["sha"] = NEXT_HEAD
        with self.assertRaisesRegex(sync.SyncError, "pinned branch head"):
            sync.verify_commit(HEAD, self.github)

    def test_content_is_bounded_and_decoded_as_a_regular_pinned_file(self):
        endpoint = self.github.file("Casks/agent-job-queue.json", b"{}")
        original = self.github.responses[endpoint]
        for field, value in (("type", "symlink"), ("path", "other.json"), ("encoding", "none"),
                             ("size", True), ("size", sync.MAX_METADATA + 1), ("size", 1),
                             ("content", "!!invalid!!"), ("content", "a" * (sync.MAX_METADATA * 2 + 1))):
            self.github.responses[endpoint] = {**original, field: value}
            with self.subTest(field=field), self.assertRaises(sync.SyncError):
                sync.tap_file("Casks/agent-job-queue.json", HEAD, self.github)

    def test_newer_published_descendant_is_explicitly_superseded(self):
        newer = manifest(124, "b" * 40)
        self.github.release(newer)
        self.github.tap(newer)
        endpoint = f"{SOURCE}/compare/{self.expected['commit']}...{newer['commit']}?per_page=1"
        self.github.responses[endpoint] = {"base_commit": {"sha": self.expected["commit"]},
                                           "merge_base_commit": {"sha": self.expected["commit"]},
                                           "behind_by": 0, "ahead_by": 2, "status": "ahead"}
        result, output = self.synchronize()
        self.assertEqual(result["status"], "superseded")
        self.assertEqual(result["build"], 124)
        self.assertIn("Verified Homebrew superseded at build 124", output)
        self.assertIn(endpoint, [endpoint for endpoint, _ in self.github.calls])

    def test_newer_published_build_at_same_commit_is_valid_supersession(self):
        newer = manifest(124)
        self.github.release(newer)
        self.github.tap(newer)
        endpoint = f"{SOURCE}/compare/{self.expected['commit']}...{newer['commit']}?per_page=1"
        self.github.responses[endpoint] = {"base_commit": {"sha": self.expected["commit"]},
                                           "merge_base_commit": {"sha": self.expected["commit"]},
                                           "behind_by": 0, "ahead_by": 0, "status": "identical"}
        self.assertEqual(sync.verify_tap(self.expected, self.github)["status"], "superseded")

    def test_newer_number_without_source_ancestry_is_rejected(self):
        newer = manifest(124, "b" * 40)
        original = {"base_commit": {"sha": self.expected["commit"]},
                    "merge_base_commit": {"sha": self.expected["commit"]},
                    "behind_by": 0, "ahead_by": 2, "status": "ahead"}
        endpoint = f"{SOURCE}/compare/{self.expected['commit']}...{newer['commit']}?per_page=1"
        for field, value in (("status", "diverged"), ("status", "behind"), ("behind_by", 1),
                             ("ahead_by", True), ("base_commit", None),
                             ("merge_base_commit", {"sha": "c" * 40})):
            self.github.responses[endpoint] = {**original, field: value}
            with self.subTest(field=field), self.assertRaisesRegex(sync.SyncError, "descend"):
                sync.verify_ancestry(self.expected, newer, self.github)

    def test_branch_advance_during_readback_restarts_at_the_new_pinned_head(self):
        first = copy.deepcopy(self.github.responses[REF_ENDPOINT])
        self.github.tap(self.expected, NEXT_HEAD)
        second = self.github.responses[REF_ENDPOINT]
        responses = iter([first, second, second, second])
        self.github.responses[REF_ENDPOINT] = lambda: next(responses)
        self.assertEqual(sync.verify_tap(self.expected, self.github)["commit"], NEXT_HEAD)
        commits = [endpoint for endpoint, _ in self.github.calls if "/commits/" in endpoint]
        self.assertEqual(commits, [f"{TAP}/commits/{HEAD}", f"{TAP}/commits/{NEXT_HEAD}"])

    def test_tooling_drift_is_reported_before_dispatch_with_only_fixed_filename(self):
        self.github.file("refresh.py", b"modified updater")
        with self.assertRaisesRegex(sync.SyncError, "Tap tooling differs: refresh.py; synchronize companion tap PR"):
            self.synchronize()
        self.assertNotIn(DISPATCH, [endpoint for endpoint, _ in self.github.calls])

    def test_tooling_mapping_is_portable_complete_and_supports_bounded_fixture_reads(self):
        self.assertEqual(set(sync.TOOLING), {"refresh.py", "render_cask.py", "release_metadata.py", "test-cask.rb",
                                           "test_refresh.py", ".github/workflows/refresh.yml"})
        self.assertEqual(sync.TOOLING["release_metadata.py"], PACKAGING.parents[1] / "release_metadata.py")
        with tempfile.TemporaryDirectory() as temporary:
            local = Path(temporary) / "refresh.py"
            local.write_bytes(b"reviewed fixture")
            self.github.file("refresh.py", local.read_bytes())
            sync.verify_tooling(self.github, {"refresh.py": local})
            local.write_bytes(b"x" * (sync.MAX_TOOLING + 1))
            with self.assertRaisesRegex(sync.SyncError, "size limit: refresh.py"):
                sync.verify_tooling(self.github, {"refresh.py": local})

    def test_manifest_bounds_and_duplicate_fields_are_rejected(self):
        for body in (b"x" * (sync.MAX_METADATA + 1), b'{"build": 123, "build": 124}', b"null", b"\xff",
                     b"[" * 2_000 + b"]" * 2_000):
            with self.subTest(body=body[:30]), self.assertRaises(sync.SyncError):
                sync.manifest_bytes(body)


class GitHubTransportTests(unittest.TestCase):
    def setUp(self):
        self.deadline = Mock()
        self.deadline.remaining.return_value = 0.25
        self.github = sync.GitHub(self.deadline, actions_token="private-app-token", source_token="private-source-token")

    def test_token_routing_version_fixed_host_and_subprocess_deadline(self):
        def complete(arguments, **options):
            options["stdout"].write(b'{}')
            self.assertLessEqual(options["timeout"], 0.25)
            self.assertEqual(options["stderr"], subprocess.DEVNULL)
            self.assertNotIn("GH_DEBUG", options["env"])
            self.assertNotIn("GITHUB_TOKEN", options["env"])
            self.assertNotIn("SOURCE_GH_TOKEN", options["env"])
            self.assertEqual(options["env"]["GH_HOST"], "github.com")
            self.assertIn(f"X-GitHub-Api-Version: {sync.API_VERSION}", arguments)
            self.assertEqual(arguments[:4], ["gh", "api", "--hostname", "github.com"])
            self.assertNotIn("private-app-token", " ".join(arguments))
            self.assertNotIn("private-source-token", " ".join(arguments))
            return SimpleNamespace(returncode=0)

        with patch.dict(os.environ, {"GH_DEBUG": "api", "GITHUB_TOKEN": "discard-token"}), \
                patch.object(sync.subprocess, "run", side_effect=complete) as run:
            self.github.json(DISPATCH, actions=True, data={"ref": "tap"})
            self.assertEqual(run.call_args.kwargs["env"]["GH_TOKEN"], "private-app-token")
            self.assertEqual(json.loads(run.call_args.kwargs["input"]), {"ref": "tap"})
            self.github.json(REF_ENDPOINT)
            self.assertEqual(run.call_args.kwargs["env"]["GH_TOKEN"], "private-source-token")
            self.assertIsNone(run.call_args.kwargs["input"])

    def test_api_failure_timeouts_and_unavailable_cli_do_not_echo_secrets(self):
        failures = [SimpleNamespace(returncode=1), subprocess.TimeoutExpired(["private-token"], 1),
                    OSError("private-token")]
        for failure in failures:
            with self.subTest(failure=failure):
                options = {"side_effect": failure} if isinstance(failure, Exception) else {"return_value": failure}
                with patch.object(sync.subprocess, "run", **options), self.assertRaises(sync.SyncError) as raised:
                    self.github.json(REF_ENDPOINT)
                self.assertNotIn("private-token", str(raised.exception))

    def test_api_output_limits_and_total_deadline_apply_after_subprocess(self):
        def complete(arguments, **options):
            options["stdout"].write(b"12345")
            return SimpleNamespace(returncode=0)

        with patch.object(sync.subprocess, "run", side_effect=complete):
            with self.assertRaisesRegex(sync.SyncError, "size limit"):
                self.github.request(REF_ENDPOINT, limit=4)
            self.deadline.remaining.side_effect = [0.25, sync.SyncError("deadline")]
            with self.assertRaisesRegex(sync.SyncError, "deadline"):
                self.github.request(REF_ENDPOINT)

    def test_token_pair_is_required_and_cli_durations_are_bounded(self):
        for actions, source in (("", "read"), ("write", "")):
            with self.subTest(actions=actions, source=source), self.assertRaises(sync.SyncError):
                sync.GitHub(self.deadline, actions_token=actions, source_token=source)
        parser = sync.bounded_seconds(1, 60)
        self.assertEqual(parser("60"), 60)
        for value in ("0", "-1", "61", "1.5", "nan", "inf"):
            with self.subTest(value=value), self.assertRaises(Exception) as raised:
                parser(value)
            self.assertIsInstance(raised.exception, sync.argparse.ArgumentTypeError)

    def test_deadline_uses_monotonic_time_and_clamps_sleep_to_remaining_budget(self):
        with patch.object(sync.time, "monotonic", side_effect=[100, 108, 110]), patch.object(sync.time, "sleep") as sleep:
            deadline = sync.Deadline(10)
            deadline.pause(5)
            sleep.assert_called_once_with(2)
            with self.assertRaisesRegex(sync.SyncError, "deadline"):
                deadline.remaining()

    def test_cli_uses_bounded_manifest_and_returns_without_a_private_path_on_read_error(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "private-local-manifest.json"
            arguments = ["sync.py", "--manifest", str(path), "--timeout", "10", "--poll-interval", "2"]
            with patch.object(sys, "argv", arguments), redirect_stderr(io.StringIO()) as output:
                self.assertEqual(sync.main(), 1)
            self.assertNotIn(temporary, output.getvalue())
            self.assertIn("Could not read required synchronization metadata", output.getvalue())
            path.write_bytes(b"x" * (sync.MAX_METADATA + 1))
            with patch.object(sys, "argv", arguments), redirect_stderr(io.StringIO()) as output:
                self.assertEqual(sync.main(), 1)
            self.assertIn("size limit", output.getvalue())

    def test_cli_passes_distinct_tokens_and_only_returns_success_after_verification(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "manifest.json"
            path.write_text(json.dumps(manifest()))
            arguments = ["sync.py", "--manifest", str(path), "--timeout", "10", "--poll-interval", "2"]
            with patch.object(sys, "argv", arguments), \
                    patch.dict(os.environ, {"GH_TOKEN": "app-token", "SOURCE_GH_TOKEN": "source-token"}), \
                    patch.object(sync, "GitHub") as github, patch.object(sync, "synchronize") as synchronize:
                self.assertEqual(sync.main(), 0)
                self.assertEqual(github.call_args.kwargs, {"actions_token": "app-token", "source_token": "source-token"})
                synchronize.assert_called_once_with(manifest(), github.return_value, poll_interval=2)
                synchronize.side_effect = sync.SyncError("Controlled verification failure")
                with redirect_stderr(io.StringIO()) as output:
                    self.assertEqual(sync.main(), 1)
                self.assertEqual(output.getvalue().strip(), "Controlled verification failure")


if __name__ == "__main__":
    unittest.main()
