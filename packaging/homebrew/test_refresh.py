"""Private, network-free release reconciliation and signed publication checks."""
from __future__ import annotations

import base64
import copy
import hashlib
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import refresh
from render_cask import render_cask, validate_manifest

HEAD = "a" * 40
PUBLISHED_HEAD = "b" * 40


def manifest(build, commit=None):
    commit = commit or str(build) * 40
    return {"schemaVersion": 1, "repository": refresh.REPOSITORY,
            "tag": f"build-{build}-{commit[:7]}", "commit": commit, "version": "0.1.0", "build": build,
            "asset": "Agent-Job-Queue-Installer-0.1.0-universal.zip",
            "sha256": hashlib.sha256(f"archive-{build}".encode()).hexdigest()}


def release(data):
    return {"tag_name": data["tag"], "target_commitish": data["commit"], "draft": False,
            "assets": [{"name": "agent-job-queue.json"}, {"name": data["asset"]},
                       {"name": "agent-job-queue.rb", "size": len(render_cask(data).encode()),
                        "digest": "sha256:" + hashlib.sha256(render_cask(data).encode()).hexdigest()}]}


class RefreshTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="tap-refresh-test-")
        self.previous = Path.cwd()
        os.chdir(self.temporary.name)
        self.addCleanup(self.temporary.cleanup)
        self.addCleanup(os.chdir, self.previous)
        self.data = {item["tag"]: item for item in (manifest(1), manifest(2), manifest(3))}
        self.source = {tag: release(data) for tag, data in self.data.items()}
        self.historical = {tag: render_cask(data).encode() for tag, data in self.data.items()}
        self.downloads = []
        self.listed = [self.source[manifest(2)["tag"]], self.source[manifest(1)["tag"]]]
        self.brew_calls = []
        self.api_calls = []
        self.remote_head = HEAD
        self.bad_archive = False
        self.api_patch = patch.object(refresh, "api", side_effect=self.api)
        self.command_patch = patch.object(refresh, "command", side_effect=self.command)
        self.publish_patch = patch.object(refresh, "publish")
        self.api_mock = self.api_patch.start()
        self.command_patch.start()
        self.publisher = self.publish_patch.start()
        self.addCleanup(self.api_patch.stop)
        self.addCleanup(self.command_patch.stop)
        self.addCleanup(self.publish_patch.stop)

    def api(self, endpoint):
        self.api_calls.append(endpoint)
        if endpoint == f"repos/{refresh.TAP_REPOSITORY}/git/ref/heads/tap":
            return {"object": {"sha": self.remote_head}}
        if endpoint == f"repos/{refresh.REPOSITORY}/releases?per_page=100&page=1":
            return copy.deepcopy(self.listed)
        compare_prefix = f"repos/{refresh.REPOSITORY}/compare/"
        if endpoint.startswith(compare_prefix):
            older, _newer = endpoint.removeprefix(compare_prefix).split("...")
            return {"status": "ahead", "merge_base_commit": {"sha": older}}
        prefix = f"repos/{refresh.REPOSITORY}/releases/tags/"
        if endpoint.startswith(prefix):
            tag = endpoint.removeprefix(prefix)
            if tag not in self.source:
                raise RuntimeError("Public release API lookup failed")
            return copy.deepcopy(self.source[tag])
        raise AssertionError(f"Unexpected API call: {endpoint}")

    def command(self, arguments, **_kwargs):
        if arguments == ["git", "rev-parse", "HEAD"]:
            return HEAD + "\n"
        if arguments[:3] == ["gh", "release", "download"]:
            tag = arguments[3]
            data = self.data[tag]
            name = arguments[arguments.index("--pattern") + 1]
            destination = Path(arguments[arguments.index("--dir") + 1]) / name
            self.downloads.append((tag, name))
            if name == "agent-job-queue.json":
                body = json.dumps(data).encode()
            elif name == "agent-job-queue.rb":
                body = self.historical[tag]
            else:
                body = f"archive-{data['build']}".encode()
                if self.bad_archive:
                    body += b"corrupt"
            destination.write_bytes(body)
            return ""
        if arguments[:2] == ["brew", "ruby"]:
            self.brew_calls.append(arguments)
            self.assertTrue(Path(arguments[-1]).read_text().startswith('cask "agent-job-queue"'))
            return ""
        raise AssertionError(f"Unexpected command: {arguments}")

    def install_current(self, data):
        Path("Casks").mkdir()
        Path("Casks/agent-job-queue.json").write_text(json.dumps(data))
        Path("Casks/agent-job-queue.rb").write_text(render_cask(data))

    def test_paired_expected_inputs_are_validated_before_any_io(self):
        for tag, commit in [(manifest(1)["tag"], ""), ("", manifest(1)["commit"]),
                            ("invalid", manifest(1)["commit"]), (manifest(1)["tag"], "f" * 40)]:
            with self.subTest(tag=tag, commit=commit), self.assertRaises(ValueError):
                refresh.refresh(publish_changes=True, expected_tag=tag, expected_commit=commit)
        self.api_mock.assert_not_called()
        self.publisher.assert_not_called()

    def test_exact_idempotence_validates_installer_and_cask(self):
        data = manifest(2)
        self.install_current(data)
        refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_not_called()
        self.assertEqual(len(self.brew_calls), 1)
        self.assertIn(f"repos/{refresh.REPOSITORY}/releases/tags/{data['tag']}", self.api_calls)

    def test_dry_run_validates_without_publication(self):
        refresh.refresh(publish_changes=False)
        self.publisher.assert_not_called()
        self.assertEqual(len(self.brew_calls), 1)

    def test_empty_tap_publishes_latest_number_not_list_order(self):
        self.listed.reverse()
        refresh.refresh(publish_changes=True)
        self.publisher.assert_called_once_with(manifest(2), render_cask(manifest(2)), HEAD)

    def test_higher_release_supersedes_verified_older_request(self):
        self.install_current(manifest(2))
        self.listed.append(self.source[manifest(3)["tag"]])
        data = manifest(1)
        refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_called_once_with(manifest(3), render_cask(manifest(3)), HEAD)
        self.assertEqual(len(self.brew_calls), 3)

    def test_unknown_expected_release_is_not_silently_superseded(self):
        data = manifest(4)
        with self.assertRaisesRegex(RuntimeError, "lookup failed"):
            refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_not_called()

    def test_expected_commit_must_match_full_public_commit(self):
        data = manifest(1)
        with self.assertRaisesRegex(ValueError, "Expected commit"):
            refresh.refresh(publish_changes=True, expected_tag=data["tag"],
                            expected_commit=data["commit"][:7] + "f" * 33)
        self.publisher.assert_not_called()

    def test_expected_release_cannot_be_newer_than_latest_even_on_noop(self):
        self.install_current(manifest(2))
        data = manifest(3)
        with self.assertRaisesRegex(ValueError, "older than the requested"):
            refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_not_called()

    def test_same_build_request_cannot_be_replaced_by_another_identity(self):
        requested = manifest(2)
        different = manifest(2, "c" * 40)
        self.data[different["tag"]] = different
        self.source[different["tag"]] = release(different)
        self.listed = [release(different)]
        with self.assertRaisesRegex(ValueError, "conflicting public release identities"):
            refresh.refresh(publish_changes=True, expected_tag=requested["tag"], expected_commit=requested["commit"])
        self.publisher.assert_not_called()

    def test_reviewed_template_update_does_not_block_upgrade_from_historical_cask(self):
        self.install_current(manifest(1))
        canonical = render_cask(manifest(2)) + "# reviewed template update\n"
        with patch.object(refresh, "render_cask", side_effect=lambda data: render_cask(data) + "# reviewed template update\n"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_called_once_with(manifest(2), canonical, HEAD)
        self.assertIn((manifest(1)["tag"], "agent-job-queue.rb"), self.downloads)
        for arguments in self.brew_calls:
            self.assertNotIn("published-cask", arguments[-1])

    def test_historical_ruby_is_only_compared_as_data_and_never_executed(self):
        data = manifest(1)
        self.install_current(data)
        historical = b"raise 'never execute downloaded Ruby'\n"
        self.historical[data["tag"]] = historical
        Path("Casks/agent-job-queue.rb").write_bytes(historical)
        asset = next(asset for asset in self.source[data["tag"]]["assets"] if asset["name"] == "agent-job-queue.rb")
        asset.update(size=len(historical), digest="sha256:" + hashlib.sha256(historical).hexdigest())
        refresh.refresh(publish_changes=True)
        self.publisher.assert_called_once_with(manifest(2), render_cask(manifest(2)), HEAD)
        self.assertEqual(len(self.brew_calls), 2)
        self.assertTrue(all("published-cask" not in arguments[-1] for arguments in self.brew_calls))

    def test_successive_template_changes_retain_same_build_until_new_release(self):
        data = manifest(2)
        self.install_current(data)
        original_cask = render_cask(data)

        def publish(data, cask, _head):
            Path("Casks/agent-job-queue.rb").write_text(cask)
            Path("Casks/agent-job-queue.json").write_text(json.dumps(data))

        self.publisher.side_effect = publish
        with patch("builtins.print") as printed:
            for suffix in ("# reviewed template B\n", "# reviewed template C\n"):
                with patch.object(refresh, "render_cask", side_effect=lambda data: render_cask(data) + suffix):
                    refresh.refresh(publish_changes=True)
                self.publisher.assert_not_called()
                self.assertEqual(Path("Casks/agent-job-queue.rb").read_text(), original_cask)
                self.assertEqual(json.loads(Path("Casks/agent-job-queue.json").read_text()), data)
            self.listed.append(self.source[manifest(3)["tag"]])
            with patch.object(refresh, "render_cask", side_effect=lambda data: render_cask(data) + suffix):
                refresh.refresh(publish_changes=True)
                refresh.refresh(publish_changes=True)
        canonical = render_cask(manifest(3)) + suffix
        self.publisher.assert_called_once_with(manifest(3), canonical, HEAD)
        messages = [call.args[0] for call in printed.call_args_list]
        self.assertEqual(messages.count("Published build verified; template change awaits a newer app release."), 2)
        self.assertEqual(self.downloads.count((data["tag"], "agent-job-queue.rb")), 3)
        self.assertEqual(json.loads(Path("Casks/agent-job-queue.json").read_text()), manifest(3))

    def test_historical_cask_without_optional_api_digest_can_be_retained(self):
        data = manifest(2)
        self.install_current(data)
        asset = next(asset for asset in self.source[data["tag"]]["assets"] if asset["name"] == "agent-job-queue.rb")
        del asset["digest"]
        canonical = render_cask(data) + "# reviewed template update\n"
        with patch.object(refresh, "render_cask", return_value=canonical):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()
        self.assertEqual(Path("Casks/agent-job-queue.rb").read_bytes(), self.historical[data["tag"]])

    def test_unknown_current_cask_is_rejected_even_when_historical_asset_is_valid(self):
        data = manifest(2)
        self.install_current(data)
        with Path("Casks/agent-job-queue.rb").open("a") as handle:
            handle.write("# unknown local change\n")
        canonical = render_cask(data) + "# reviewed template update\n"
        with patch.object(refresh, "render_cask", return_value=canonical), \
                self.assertRaisesRegex(ValueError, "cask differs from its historical"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_historical_cask_digest_mismatch_fails(self):
        data = manifest(2)
        self.install_current(data)
        self.historical[data["tag"]] = self.historical[data["tag"]].replace(b"Shared heavy-command", b"Edited heavy-command")
        canonical = render_cask(data) + "# reviewed template update\n"
        with patch.object(refresh, "render_cask", return_value=canonical), \
                self.assertRaisesRegex(ValueError, "asset digest does not match"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_historical_asset_metadata_must_be_unique_bounded_and_have_supported_digest(self):
        data = manifest(2)
        self.install_current(data)
        canonical = render_cask(data) + "# reviewed template update\n"
        original_assets = copy.deepcopy(self.source[data["tag"]]["assets"])
        original = next(asset for asset in original_assets if asset["name"] == "agent-job-queue.rb")
        other_assets = [asset for asset in original_assets if asset["name"] != "agent-job-queue.rb"]
        invalid = [[], [original, original], [original | {"size": refresh.MAX_CASK_BYTES + 1}],
                   [original | {"size": True}], [original | {"size": 0}], [original | {"size": None}],
                   [original | {"digest": "sha1:" + "1" * 40}], [original | {"digest": ""}]]
        for historical in invalid:
            self.source[data["tag"]]["assets"] = other_assets + historical
            with self.subTest(historical=historical), patch.object(refresh, "render_cask", return_value=canonical), \
                    self.assertRaisesRegex(ValueError, "Historical public"):
                refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_actual_historical_cask_must_match_declared_bounded_size(self):
        data = manifest(2)
        self.install_current(data)
        canonical = render_cask(data) + "# reviewed template update\n"
        for body in (b"", b"short", b"x" * (refresh.MAX_CASK_BYTES + 1)):
            self.historical[data["tag"]] = body
            with self.subTest(size=len(body)), patch.object(refresh, "render_cask", return_value=canonical), \
                    self.assertRaisesRegex(ValueError, "declared bounded size"):
                refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_manifest_or_archive_failure_precedes_historical_cask_download(self):
        data = manifest(2)
        self.install_current(data)
        self.bad_archive = True
        canonical = render_cask(data) + "# reviewed template update\n"
        with patch.object(refresh, "render_cask", return_value=canonical), self.assertRaisesRegex(ValueError, "checksum"):
            refresh.refresh(publish_changes=True)
        self.assertNotIn((data["tag"], "agent-job-queue.rb"), self.downloads)
        self.publisher.assert_not_called()

    def test_same_build_manifest_drift_fails(self):
        current = manifest(2)
        current["sha256"] = "f" * 64
        self.install_current(current)
        with self.assertRaisesRegex(ValueError, "differs from its validated public release"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_same_build_cask_drift_fails(self):
        self.install_current(manifest(2))
        with Path("Casks/agent-job-queue.rb").open("a") as handle:
            handle.write("# unexpected edit\n")
        with self.assertRaisesRegex(ValueError, "cask differs"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_same_build_different_public_identity_fails(self):
        current = manifest(2)
        self.install_current(current)
        different = manifest(2, "c" * 40)
        self.data[different["tag"]] = different
        self.source[different["tag"]] = release(different)
        self.listed = [release(different)]
        with self.assertRaisesRegex(ValueError, "same build number"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_current_release_missing_from_source_fails(self):
        self.install_current(manifest(1))
        del self.source[manifest(1)["tag"]]
        with self.assertRaisesRegex(RuntimeError, "lookup failed"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_newer_current_tap_is_never_downgraded(self):
        self.install_current(manifest(3))
        with self.assertRaisesRegex(ValueError, "downgrade"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_unpaired_current_cask_or_manifest_fails(self):
        Path("Casks").mkdir()
        Path("Casks/agent-job-queue.rb").write_text("invalid")
        with self.assertRaisesRegex(ValueError, "both exist"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_current_noop_still_rejects_changed_archive_bytes(self):
        self.install_current(manifest(2))
        self.bad_archive = True
        with self.assertRaisesRegex(ValueError, "checksum"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_draft_expected_release_is_rejected(self):
        data = manifest(1)
        self.source[data["tag"]]["draft"] = True
        with self.assertRaisesRegex(ValueError, "Expected public release"):
            refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_not_called()

    def test_release_target_commit_must_equal_manifest(self):
        self.source[manifest(2)["tag"]]["target_commitish"] = "main"
        with self.assertRaisesRegex(ValueError, "identity does not match"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_duplicate_expected_assets_are_rejected(self):
        self.source[manifest(2)["tag"]]["assets"].append({"name": "agent-job-queue.json"})
        with self.assertRaisesRegex(ValueError, "unique expected"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_newer_build_from_unrelated_source_cannot_supersede_current_or_expected(self):
        self.install_current(manifest(1))
        data = manifest(1)
        for status in ("behind", "diverged", "identical", "ahead"):
            original = self.api

            def api(endpoint):
                if "/compare/" in endpoint:
                    return {"status": status, "merge_base_commit": {"sha": "f" * 40}}
                return original(endpoint)

            with self.subTest(status=status), patch.object(refresh, "api", side_effect=api), \
                    self.assertRaisesRegex(ValueError, "verified descendant"):
                refresh.refresh(publish_changes=True, expected_tag=data["tag"], expected_commit=data["commit"])
        self.publisher.assert_not_called()

    def test_current_successor_ancestry_is_verified_without_explicit_request(self):
        self.install_current(manifest(1))
        original = self.api

        def api(endpoint):
            if "/compare/" in endpoint:
                return {"status": "behind", "merge_base_commit": {"sha": manifest(1)["commit"]}}
            return original(endpoint)

        with patch.object(refresh, "api", side_effect=api), self.assertRaisesRegex(ValueError, "verified descendant"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_homebrew_dsl_failure_blocks_publication(self):
        original = self.command

        def command(arguments, **kwargs):
            if arguments[:2] == ["brew", "ruby"]:
                raise RuntimeError("Homebrew DSL rejected generated cask")
            return original(arguments, **kwargs)

        with patch.object(refresh, "command", side_effect=command), self.assertRaisesRegex(RuntimeError, "DSL"):
            refresh.refresh(publish_changes=True)
        self.publisher.assert_not_called()

    def test_stale_checkout_fails_before_source_download(self):
        self.remote_head = "c" * 40
        with self.assertRaisesRegex(RuntimeError, "checkout is stale"):
            refresh.refresh(publish_changes=True)
        self.assertEqual(len(self.api_calls), 1)
        self.publisher.assert_not_called()


class ReleaseSelectionTests(unittest.TestCase):
    def test_page_two_is_searched_and_numeric_build_wins(self):
        pages = [[release(manifest(1))] * 100, [release(manifest(3)), release(manifest(2))]]
        with patch.object(refresh, "api", side_effect=pages) as api:
            selected = refresh.select_release(refresh.published_releases())
        self.assertEqual(selected["tag_name"], manifest(3)["tag"])
        self.assertEqual(api.call_args_list[-1].args[0],
                         f"repos/{refresh.REPOSITORY}/releases?per_page=100&page=2")

    def test_page_bound_fails_instead_of_selecting_from_truncated_history(self):
        with patch.object(refresh, "MAX_RELEASE_PAGES", 2), \
                patch.object(refresh, "api", return_value=[release(manifest(1))] * 100) as api, \
                self.assertRaisesRegex(ValueError, "page bound"):
            refresh.published_releases()
        self.assertEqual(api.call_count, 2)

    def test_bad_page_shape_and_api_errors_are_visible(self):
        for page in ({"message": "error"}, [None], [release(manifest(1))] * 101):
            with self.subTest(page=type(page)), patch.object(refresh, "api", return_value=page), \
                    self.assertRaisesRegex(ValueError, "Unexpected source"):
                refresh.published_releases()
        with patch.object(refresh, "api", side_effect=RuntimeError("rate limit")), \
                self.assertRaisesRegex(RuntimeError, "rate limit"):
            refresh.published_releases()

    def test_no_public_metadata_release_fails(self):
        draft = release(manifest(2))
        draft["draft"] = True
        missing = release(manifest(1))
        missing["assets"] = []
        with self.assertRaisesRegex(ValueError, "No published"):
            refresh.select_release([draft, missing, {"tag_name": "v0.1.0"}])

    def test_duplicate_latest_build_identity_is_ambiguous(self):
        with self.assertRaisesRegex(ValueError, "Multiple public releases"):
            refresh.select_release([release(manifest(2)), release(manifest(2, "c" * 40))])

    def test_newer_build_of_the_same_commit_needs_no_ancestry_lookup(self):
        with patch.object(refresh, "api") as api:
            refresh.verify_successor(manifest(1), manifest(2, manifest(1)["commit"]))
        api.assert_not_called()

    def test_ancestry_api_error_remains_visible(self):
        with patch.object(refresh, "api", side_effect=RuntimeError("compare unavailable")), \
                self.assertRaisesRegex(RuntimeError, "compare unavailable"):
            refresh.verify_successor(manifest(1), manifest(2))

    def test_strict_manifest_rejects_unexpected_source_and_fields(self):
        for changes in ({"repository": "other/repository"}, {"build": True}, {"tag": "build-2-1111111"},
                        {"asset": "../../installer.zip"}, {"extra": "unsupported"}, {"sha256": "not-a-digest"}):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                validate_manifest(manifest(2) | changes)


class PublicationTests(unittest.TestCase):
    def graphql(self, _query, variables):
        if not variables:
            return {"viewer": {"login": refresh.BOT_NAME}}
        return {"createCommitOnBranch": {"commit": {"oid": PUBLISHED_HEAD}}}

    def commit(self, **changes):
        result = {"author": {"name": refresh.BOT_NAME, "email": refresh.BOT_EMAIL},
                  "message": f"chore(cask): publish build 2\n\nSigned-off-by: {refresh.BOT_NAME} <{refresh.BOT_EMAIL}>",
                  "verification": {"verified": True}}
        return {"commit": result | changes}

    def test_publication_uses_signed_api_commit_with_cas_and_matching_dco(self):
        data = manifest(2)
        with patch.object(refresh, "graphql", side_effect=self.graphql) as graphql, \
                patch.object(refresh, "api", return_value=self.commit()) as api:
            refresh.publish(data, render_cask(data), HEAD)
        payload = graphql.call_args_list[1].args[1]["input"]
        self.assertEqual(payload["expectedHeadOid"], HEAD)
        self.assertEqual(payload["branch"], {"repositoryNameWithOwner": refresh.TAP_REPOSITORY, "branchName": "tap"})
        self.assertEqual(payload["message"]["body"], f"Signed-off-by: {refresh.BOT_NAME} <{refresh.BOT_EMAIL}>")
        files = {item["path"]: base64.b64decode(item["contents"]).decode()
                 for item in payload["fileChanges"]["additions"]}
        self.assertEqual(files["Casks/agent-job-queue.rb"], render_cask(data))
        self.assertEqual(json.loads(files["Casks/agent-job-queue.json"]), data)
        api.assert_called_once_with(f"repos/{refresh.TAP_REPOSITORY}/commits/{PUBLISHED_HEAD}")

    def test_cas_rejection_propagates_without_retry_or_overwrite(self):
        with patch.object(refresh, "graphql", side_effect=[{"viewer": {"login": refresh.BOT_NAME}},
                                                           RuntimeError("expectedHeadOid mismatch")]) as graphql, \
                patch.object(refresh, "api") as api, self.assertRaisesRegex(RuntimeError, "expectedHeadOid"):
            refresh.publish(manifest(2), render_cask(manifest(2)), HEAD)
        self.assertEqual(graphql.call_count, 2)
        api.assert_not_called()

    def test_non_bot_cannot_publish(self):
        with patch.object(refresh, "graphql", return_value={"viewer": {"login": "human"}}) as graphql, \
                self.assertRaisesRegex(ValueError, "Actions token"):
            refresh.publish(manifest(2), render_cask(manifest(2)), HEAD)
        self.assertEqual(graphql.call_count, 1)

    def test_bad_signature_author_or_dco_fails_after_publication(self):
        for changes in ({"verification": {"verified": False}}, {"message": "missing DCO"},
                        {"author": {"name": "human", "email": refresh.BOT_EMAIL}}):
            with self.subTest(changes=changes), patch.object(refresh, "graphql", side_effect=self.graphql), \
                    patch.object(refresh, "api", return_value=self.commit(**changes)), \
                    self.assertRaisesRegex(RuntimeError, "Verified signature or matching DCO"):
                refresh.publish(manifest(2), render_cask(manifest(2)), HEAD)

    def test_main_publish_is_restricted_to_tap_schedule_or_dispatch(self):
        valid = {"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": refresh.TAP_REPOSITORY,
                 "GITHUB_REF": "refs/heads/tap", "GITHUB_EVENT_NAME": "workflow_dispatch"}
        cases = [{}, valid | {"GITHUB_REF": "refs/heads/main"}, valid | {"GITHUB_EVENT_NAME": "pull_request"},
                 valid | {"GITHUB_REPOSITORY": refresh.REPOSITORY}, valid | {"GITHUB_ACTIONS": "false"}]
        for environment in cases:
            with self.subTest(environment=environment), patch.dict(os.environ, environment, clear=True), \
                    patch("sys.argv", ["refresh.py", "--publish"]), patch.object(refresh, "refresh") as run, \
                    self.assertRaisesRegex(ValueError, "restricted to tap"):
                refresh.main()
            run.assert_not_called()
        for event in ("workflow_dispatch", "schedule"):
            with patch.dict(os.environ, valid | {"GITHUB_EVENT_NAME": event}, clear=True), \
                    patch("sys.argv", ["refresh.py", "--publish"]), patch.object(refresh, "refresh") as run:
                refresh.main()
            run.assert_called_once_with(publish_changes=True, expected_tag="", expected_commit="")


if __name__ == "__main__":
    unittest.main()
