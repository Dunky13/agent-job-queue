#!/usr/bin/env python3
"""Dispatch the dedicated tap refresh and independently verify its published cask."""
from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from render_cask import REPOSITORY, render_cask, validate_manifest

TAP_REPOSITORY = "Dunky13/homebrew-agent-job-queue"
BRANCH = "tap"
WORKFLOW = "refresh.yml"
API_VERSION = "2026-03-10"
MAX_METADATA = 16_384
MAX_API_RESPONSE = 2_097_152
MAX_TOOLING = 131_072
SHA = re.compile(r"[0-9a-f]{40}\Z")
PACKAGING = Path(__file__).resolve().parent
TOOLING = {"refresh.py": PACKAGING / "refresh.py", "render_cask.py": PACKAGING / "render_cask.py",
           "release_metadata.py": PACKAGING.parents[1] / "release_metadata.py",
           "test-cask.rb": PACKAGING / "test-cask.rb", "test_refresh.py": PACKAGING / "test_refresh.py",
           ".github/workflows/refresh.yml": PACKAGING / ".github/workflows/refresh.yml"}


class SyncError(RuntimeError):
    """A safe error that never contains an API response or a credential."""


class Deadline:
    def __init__(self, seconds: int):
        self.end = time.monotonic() + seconds

    def remaining(self) -> float:
        remaining = self.end - time.monotonic()
        if remaining <= 0:
            raise SyncError("Homebrew synchronization exceeded its deadline")
        return remaining

    def pause(self, seconds: int):
        time.sleep(min(seconds, self.remaining()))


def parse_json(body: bytes):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("Duplicate JSON field")
            result[key] = value
        return result

    try:
        return json.loads(body, object_pairs_hook=unique_object)
    except (ValueError, UnicodeError, RecursionError) as from_error:
        raise SyncError("GitHub metadata is not valid JSON") from from_error


class GitHub:
    """Use gh's authentication and transport, with separate dispatch and read tokens."""

    def __init__(self, deadline: Deadline, *, actions_token: str, source_token: str):
        if not actions_token or not source_token:
            raise SyncError("GH_TOKEN and SOURCE_GH_TOKEN must both be configured")
        self.deadline = deadline
        self.actions_token = actions_token
        self.source_token = source_token

    def request(self, endpoint: str, *, actions=False, data=None, raw=False, limit=MAX_API_RESPONSE) -> bytes:
        arguments = ["gh", "api", "--hostname", "github.com", "--method", "POST" if data is not None else "GET",
                     "-H", f"X-GitHub-Api-Version: {API_VERSION}", "-H",
                     "Accept: application/octet-stream" if raw else "Accept: application/vnd.github+json", endpoint]
        if data is not None:
            arguments.extend(["--input", "-"])
        environment = {key: value for key, value in os.environ.items()
                       if key not in {"GH_TOKEN", "GITHUB_TOKEN", "GH_DEBUG", "SOURCE_GH_TOKEN"}}
        environment.update(GH_TOKEN=self.actions_token if actions else self.source_token, GH_HOST="github.com")
        try:
            with tempfile.TemporaryFile() as output:
                result = subprocess.run(arguments, input=json.dumps(data).encode() if data is not None else None,
                                        stdout=output, stderr=subprocess.DEVNULL, env=environment,
                                        timeout=min(60, self.deadline.remaining()), check=False)
                if result.returncode:
                    raise SyncError(f"Required GitHub API operation failed (exit {result.returncode})")
                output.seek(0)
                body = output.read(limit + 1)
        except subprocess.TimeoutExpired as from_error:
            raise SyncError("GitHub API operation timed out during Homebrew synchronization") from from_error
        except OSError as from_error:
            raise SyncError("GitHub CLI could not complete the required API operation") from from_error
        self.deadline.remaining()
        if len(body) > limit:
            raise SyncError("GitHub metadata exceeds its size limit")
        return body

    def json(self, endpoint: str, **options):
        return parse_json(self.request(endpoint, **options))


def positive_id(value) -> bool:
    return type(value) is int and 1 <= value <= 9_223_372_036_854_775_807


def manifest_bytes(body: bytes) -> dict:
    if len(body) > MAX_METADATA:
        raise SyncError("Release manifest exceeds its size limit")
    try:
        return validate_manifest(parse_json(body))
    except (ValueError, TypeError, KeyError) as from_error:
        raise SyncError("Release manifest has an invalid immutable identity") from from_error


def verify_release(data: dict, github: GitHub):
    """Read a public, published known-source release and its exact bounded manifest."""
    release = github.json(f"repos/{REPOSITORY}/releases/tags/{data['tag']}")
    if (not isinstance(release, dict) or release.get("draft") is not False
            or release.get("tag_name") != data["tag"] or release.get("target_commitish") != data["commit"]
            or not isinstance(release.get("published_at"), str) or not release["published_at"]):
        raise SyncError("Published source release does not match the requested immutable identity")
    assets = release.get("assets")
    if not isinstance(assets, list) or len(assets) > 100:
        raise SyncError("Published source release has invalid assets")
    selected = {}
    for name in ("agent-job-queue.json", data["asset"]):
        matches = [asset for asset in assets if isinstance(asset, dict) and asset.get("name") == name]
        if len(matches) != 1:
            raise SyncError("Published source release is missing a unique required asset")
        asset = matches[0]
        if (not positive_id(asset.get("id")) or asset.get("state") != "uploaded"
                or type(asset.get("size")) is not int or asset["size"] <= 0):
            raise SyncError("Published source release contains an invalid required asset")
        selected[name] = asset
    if selected[data["asset"]].get("digest") != f"sha256:{data['sha256']}":
        raise SyncError("Published installer asset checksum conflicts with its release manifest")
    manifest = selected["agent-job-queue.json"]
    if manifest["size"] > MAX_METADATA:
        raise SyncError("Release manifest exceeds its size limit")
    public = github.request(f"repos/{REPOSITORY}/releases/assets/{manifest['id']}", raw=True, limit=MAX_METADATA)
    if manifest.get("digest") != f"sha256:{hashlib.sha256(public).hexdigest()}":
        raise SyncError("Published source manifest checksum does not match its asset metadata")
    if len(public) != manifest["size"] or manifest_bytes(public) != data:
        raise SyncError("Published source manifest conflicts with the requested immutable identity")


def dispatch(data: dict, github: GitHub) -> int:
    response = github.json(f"repos/{TAP_REPOSITORY}/actions/workflows/{WORKFLOW}/dispatches", actions=True,
                           data={"ref": BRANCH, "inputs": {"expected_tag": data["tag"],
                                                           "expected_commit": data["commit"]}})
    if not isinstance(response, dict) or not positive_id(response.get("workflow_run_id")):
        raise SyncError("GitHub dispatch did not return an exact workflow run identifier")
    run_id = response["workflow_run_id"]
    if (response.get("run_url") != f"https://api.github.com/repos/{TAP_REPOSITORY}/actions/runs/{run_id}"
            or response.get("html_url") != run_url(run_id)):
        raise SyncError("GitHub dispatch returned an unexpected workflow run identity")
    return run_id


def run_url(run_id: int) -> str:
    return f"https://github.com/{TAP_REPOSITORY}/actions/runs/{run_id}"


def wait_for_run(run_id: int, github: GitHub, poll_interval: int):
    while True:
        run = github.json(f"repos/{TAP_REPOSITORY}/actions/runs/{run_id}", actions=True)
        if (not isinstance(run, dict) or not positive_id(run.get("id")) or run["id"] != run_id
                or not isinstance(run.get("repository"), dict) or run["repository"].get("full_name") != TAP_REPOSITORY
                or run.get("head_branch") != BRANCH or run.get("event") != "workflow_dispatch"
                or run.get("path") != f".github/workflows/{WORKFLOW}"):
            raise SyncError("GitHub returned an unexpected tap workflow run")
        if run.get("status") == "completed":
            if run.get("conclusion") != "success":
                raise SyncError(f"Tap workflow did not succeed; inspect {run_url(run_id)}")
            return
        if (not isinstance(run.get("status"), str)
                or run["status"] not in {"queued", "requested", "waiting", "pending", "in_progress"}):
            raise SyncError("Tap workflow returned an unknown execution status")
        github.deadline.pause(poll_interval)


def tap_head(github: GitHub) -> str:
    reference = github.json(f"repos/{TAP_REPOSITORY}/git/ref/heads/{BRANCH}")
    if not isinstance(reference, dict) or not isinstance(reference.get("object"), dict):
        raise SyncError("Tap branch does not identify a commit")
    sha = reference["object"].get("sha")
    if (reference.get("ref") != f"refs/heads/{BRANCH}" or reference["object"].get("type") != "commit"
            or not isinstance(sha, str) or not SHA.fullmatch(sha)):
        raise SyncError("Tap branch does not identify a valid immutable commit")
    return sha


def verify_commit(sha: str, github: GitHub):
    response = github.json(f"repos/{TAP_REPOSITORY}/commits/{sha}")
    if not isinstance(response, dict) or response.get("sha") != sha or not isinstance(response.get("commit"), dict):
        raise SyncError("Tap commit identity does not match the pinned branch head")
    commit = response["commit"]
    author, verification, message = commit.get("author"), commit.get("verification"), commit.get("message")
    if not isinstance(author, dict) or not isinstance(verification, dict) or verification.get("verified") is not True:
        raise SyncError("Tap commit does not have a verified GitHub signature")
    name, email = author.get("name"), author.get("email")
    if (any(not isinstance(value, str) or not value or len(value) > 256 or "\n" in value or "\r" in value
            for value in (name, email)) or not isinstance(message, str)
            or f"Signed-off-by: {name} <{email}>" not in message.splitlines()):
        raise SyncError("Tap commit is missing its author-matching DCO sign-off")


def tap_file(path: str, sha: str, github: GitHub, *, limit=MAX_METADATA) -> bytes:
    item = github.json(f"repos/{TAP_REPOSITORY}/contents/{path}?ref={sha}", limit=limit * 2)
    if (not isinstance(item, dict) or item.get("type") != "file" or item.get("path") != path
            or item.get("encoding") != "base64" or type(item.get("size")) is not int
            or not 0 < item["size"] <= limit or not isinstance(item.get("content"), str)
            or len(item["content"]) > limit * 2):
        raise SyncError("Pinned tap metadata is not a bounded regular file")
    try:
        body = base64.b64decode("".join(item["content"].split()), validate=True)
    except (binascii.Error, ValueError) as from_error:
        raise SyncError("Pinned tap metadata has invalid content encoding") from from_error
    if len(body) != item["size"]:
        raise SyncError("Pinned tap metadata size does not match its content")
    return body


def verify_tooling(github: GitHub, tooling=None):
    """Compare reviewed companion-tap helpers without importing downloaded code."""
    files = TOOLING if tooling is None else tooling
    while True:
        sha = tap_head(github)
        verify_commit(sha, github)
        for name, path in files.items():
            with path.open("rb") as handle:
                expected = handle.read(MAX_TOOLING + 1)
            if len(expected) > MAX_TOOLING:
                raise SyncError(f"Canonical tap tooling exceeds its size limit: {name}")
            if tap_file(name, sha, github, limit=MAX_TOOLING) != expected:
                raise SyncError(f"Tap tooling differs: {name}; synchronize companion tap PR")
        if tap_head(github) == sha:
            return


def verify_ancestry(expected: dict, current: dict, github: GitHub):
    comparison = github.json(f"repos/{REPOSITORY}/compare/{expected['commit']}...{current['commit']}?per_page=1")
    if not isinstance(comparison, dict):
        raise SyncError("Newer tap build has no verified source ancestry")
    ahead = comparison.get("ahead_by")
    status = comparison.get("status")
    if (not isinstance(comparison.get("base_commit"), dict)
            or comparison["base_commit"].get("sha") != expected["commit"]
            or not isinstance(comparison.get("merge_base_commit"), dict)
            or comparison["merge_base_commit"].get("sha") != expected["commit"]
            or type(comparison.get("behind_by")) is not int or comparison["behind_by"] != 0
            or type(ahead) is not int or not ((status == "ahead" and ahead > 0)
                                            or (status == "identical" and ahead == 0))):
        raise SyncError("Newer tap build does not descend from the requested source commit")


def verify_tap(expected: dict, github: GitHub) -> dict:
    while True:
        sha = tap_head(github)
        verify_commit(sha, github)
        current = manifest_bytes(tap_file("Casks/agent-job-queue.json", sha, github))
        if current["build"] < expected["build"]:
            raise SyncError("Tap still points to an older build after its refresh completed")
        if current["build"] == expected["build"] and current != expected:
            raise SyncError("Tap contains conflicting metadata for the requested build number")
        if tap_file("Casks/agent-job-queue.rb", sha, github) != render_cask(current).encode():
            raise SyncError("Pinned tap cask differs from the trusted source renderer")
        verify_release(current, github)
        if current["build"] > expected["build"]:
            verify_ancestry(expected, current, github)
        if tap_head(github) == sha:
            return {"status": "current" if current == expected else "superseded",
                    "build": current["build"], "commit": sha}


def synchronize(expected: dict, github: GitHub, *, poll_interval: int) -> dict:
    expected = manifest_bytes(json.dumps(expected).encode())
    verify_release(expected, github)
    verify_tooling(github)
    run_id = dispatch(expected, github)
    print(f"Dispatched tap refresh for build {expected['build']}: {run_url(run_id)}", flush=True)
    wait_for_run(run_id, github, poll_interval)
    result = verify_tap(expected, github)
    print(f"Verified Homebrew {result['status']} at build {result['build']}: "
          f"https://github.com/{TAP_REPOSITORY}/commit/{result['commit']}", flush=True)
    return result


def bounded_seconds(minimum: int, maximum: int):
    def parse(value: str):
        try:
            seconds = int(value)
        except ValueError as from_error:
            raise argparse.ArgumentTypeError("Expected a whole number of seconds") from from_error
        if not minimum <= seconds <= maximum:
            raise argparse.ArgumentTypeError(f"Seconds must be between {minimum} and {maximum}")
        return seconds
    return parse


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--timeout", type=bounded_seconds(1, 1800), default=900)
    parser.add_argument("--poll-interval", type=bounded_seconds(1, 60), default=5)
    args = parser.parse_args()
    try:
        with args.manifest.open("rb") as handle:
            expected = manifest_bytes(handle.read(MAX_METADATA + 1))
        github = GitHub(Deadline(args.timeout), actions_token=os.environ.get("GH_TOKEN", ""),
                        source_token=os.environ.get("SOURCE_GH_TOKEN", ""))
        synchronize(expected, github, poll_interval=args.poll_interval)
    except (SyncError, OSError) as error:
        # Exception chaining and raw OS/API messages can expose local paths or tokens.
        print(str(error) if isinstance(error, SyncError) else "Could not read required synchronization metadata",
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
