#!/usr/bin/env python3
"""Reconcile the generated tap with validated public app release metadata."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
import tempfile
from pathlib import Path

from render_cask import REPOSITORY, render_cask, validate_manifest

TAP_REPOSITORY = "Dunky13/homebrew-agent-job-queue"
BRANCH = "tap"
BOT_NAME = "github-actions[bot]"
BOT_EMAIL = "41898282+github-actions[bot]@users.noreply.github.com"
TAG = re.compile(r"build-([1-9][0-9]{0,8})-([0-9a-f]{7})\Z")
COMMIT = re.compile(r"[0-9a-f]{40}\Z")
RELEASES_PER_PAGE = 100
MAX_RELEASE_PAGES = 10
MAX_CASK_BYTES = 16_384


def command(arguments, *, input_data=None):
    result = subprocess.run(arguments, input=input_data, capture_output=True, text=True, timeout=120, check=False)
    if result.returncode:
        raise RuntimeError(f"Required {arguments[0]} operation failed (exit {result.returncode})")
    return result.stdout


def api(endpoint):
    return json.loads(command(["gh", "api", endpoint]))


def graphql(query, variables):
    result = json.loads(command(["gh", "api", "graphql", "--input", "-"],
                                input_data=json.dumps({"query": query, "variables": variables})))
    if result.get("errors") or "data" not in result:
        raise RuntimeError("GitHub GraphQL operation failed")
    return result["data"]


def validate_expected(tag: str, commit: str):
    if bool(tag) != bool(commit):
        raise ValueError("Expected tag and commit must be supplied together")
    if not tag and not commit:
        return None
    match = TAG.fullmatch(tag)
    if match is None or COMMIT.fullmatch(commit) is None or match[2] != commit[:7]:
        raise ValueError("Invalid expected release identity")
    return tag, commit


def published_releases():
    releases = []
    for page in range(1, MAX_RELEASE_PAGES + 1):
        batch = api(f"repos/{REPOSITORY}/releases?per_page={RELEASES_PER_PAGE}&page={page}")
        if (not isinstance(batch, list) or len(batch) > RELEASES_PER_PAGE
                or any(not isinstance(release, dict) for release in batch)):
            raise ValueError("Unexpected source release response")
        releases.extend(batch)
        if len(batch) < RELEASES_PER_PAGE:
            return releases
    raise ValueError("Public release listing exceeds its page bound; cannot prove the latest build")


def select_release(releases):
    if not isinstance(releases, list):
        raise ValueError("Unexpected source release response")
    candidates = [release for release in releases if isinstance(release, dict) and not release.get("draft")
                  and isinstance(release.get("tag_name"), str) and TAG.fullmatch(release["tag_name"])
                  and isinstance(release.get("assets"), list)
                  and any(asset.get("name") == "agent-job-queue.json" for asset in release["assets"]
                          if isinstance(asset, dict))]
    if not candidates:
        raise ValueError("No published app build with Homebrew metadata exists")
    newest = max(candidates, key=lambda release: int(TAG.fullmatch(release["tag_name"])[1]))
    build = int(TAG.fullmatch(newest["tag_name"])[1])
    if any(int(TAG.fullmatch(release["tag_name"])[1]) == build
           and release["tag_name"] != newest["tag_name"] for release in candidates):
        raise ValueError("Multiple public releases claim the latest build number")
    return newest


def release_by_tag(tag: str):
    release = api(f"repos/{REPOSITORY}/releases/tags/{tag}")
    if (not isinstance(release, dict) or release.get("draft") is not False
            or release.get("tag_name") != tag):
        raise ValueError("Expected public release is missing or has a different identity")
    return release


def verify_successor(older, newer):
    if newer["build"] <= older["build"] or newer["commit"] == older["commit"]:
        return
    comparison = api(f"repos/{REPOSITORY}/compare/{older['commit']}...{newer['commit']}")
    if (not isinstance(comparison, dict) or comparison.get("status") != "ahead"
            or not isinstance(comparison.get("merge_base_commit"), dict)
            or comparison["merge_base_commit"].get("sha") != older["commit"]):
        raise ValueError("Newer build is not a verified descendant of the previous release")


def verify_release(manifest, release, archive: Path):
    data = validate_manifest(manifest)
    if (release.get("draft") is not False or data["tag"] != release.get("tag_name")
            or data["commit"] != release.get("target_commitish")):
        raise ValueError("Release identity does not match manifest")
    if not isinstance(release.get("assets"), list):
        raise ValueError("Unexpected source release assets")
    assets = [asset.get("name") for asset in release["assets"] if isinstance(asset, dict)]
    if assets.count("agent-job-queue.json") != 1 or assets.count(data["asset"]) != 1:
        raise ValueError("Release does not contain unique expected metadata and installer assets")
    with archive.open("rb") as handle:
        checksum = hashlib.file_digest(handle, "sha256").hexdigest()
    if checksum != data["sha256"]:
        raise ValueError("Downloaded installer checksum does not match manifest")
    return data


def download_release(release, work: Path):
    tag = release["tag_name"]
    if not isinstance(tag, str) or TAG.fullmatch(tag) is None:
        raise ValueError("Invalid public release tag")
    destination = work / tag
    destination.mkdir()
    command(["gh", "release", "download", tag, "--repo", REPOSITORY, "--pattern", "agent-job-queue.json",
             "--dir", str(destination)])
    manifest_file = destination / "agent-job-queue.json"
    if manifest_file.stat().st_size > 16_384:
        raise ValueError("Release manifest exceeds its size limit")
    manifest = validate_manifest(json.loads(manifest_file.read_text()))
    command(["gh", "release", "download", tag, "--repo", REPOSITORY, "--pattern", manifest["asset"],
             "--dir", str(destination)])
    data = verify_release(manifest, release, destination / manifest["asset"])
    cask = render_cask(data)
    cask_path = destination / "agent-job-queue.rb"
    cask_path.write_text(cask)
    command(["brew", "ruby", str(Path(__file__).with_name("test-cask.rb")), str(cask_path)])
    return data, cask


def download_historical_cask(release, work: Path) -> bytes:
    assets = [asset for asset in release["assets"]
              if isinstance(asset, dict) and asset.get("name") == "agent-job-queue.rb"]
    if len(assets) != 1:
        raise ValueError("Historical public release must contain exactly one cask asset")
    asset = assets[0]
    declared_size = asset.get("size")
    if type(declared_size) is not int or not 1 <= declared_size <= MAX_CASK_BYTES:
        raise ValueError("Historical public cask exceeds its declared size limit")
    digest = asset.get("digest")
    if digest is not None and (not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest)):
        raise ValueError("Historical public cask has an unsupported asset digest")
    destination = work / release["tag_name"] / "published-cask"
    destination.mkdir()
    command(["gh", "release", "download", release["tag_name"], "--repo", REPOSITORY,
             "--pattern", "agent-job-queue.rb", "--dir", str(destination)])
    cask_path = destination / "agent-job-queue.rb"
    actual_size = cask_path.stat().st_size
    if actual_size != declared_size or actual_size > MAX_CASK_BYTES:
        raise ValueError("Historical public cask does not match its declared bounded size")
    body = cask_path.read_bytes()
    if digest is not None and f"sha256:{hashlib.sha256(body).hexdigest()}" != digest:
        raise ValueError("Historical public cask asset digest does not match downloaded bytes")
    # This release artifact is comparison data only. Never pass it to Ruby.
    return body


def publish(data, cask: str, expected_head: str):
    viewer = graphql("query { viewer { login } }", {})["viewer"]
    if viewer["login"] != BOT_NAME:
        raise ValueError("Automatic tap publication requires the repository's GitHub Actions token")
    files = {"Casks/agent-job-queue.rb": cask, "Casks/agent-job-queue.json": json.dumps(data, indent=2) + "\n"}
    mutation = """mutation($input: CreateCommitOnBranchInput!) {
      createCommitOnBranch(input: $input) { commit { oid } }
    }"""
    variables = {"input": {
        "branch": {"repositoryNameWithOwner": TAP_REPOSITORY, "branchName": BRANCH},
        "expectedHeadOid": expected_head,
        "message": {"headline": f"chore(cask): publish build {data['build']}",
                    "body": f"Signed-off-by: {BOT_NAME} <{BOT_EMAIL}>"},
        "fileChanges": {"additions": [{"path": path, "contents": base64.b64encode(body.encode()).decode()}
                                       for path, body in files.items()]},
    }}
    commit = graphql(mutation, variables)["createCommitOnBranch"]["commit"]["oid"]
    if not isinstance(commit, str) or COMMIT.fullmatch(commit) is None:
        raise RuntimeError("Tap publication returned an invalid commit identity")
    result = api(f"repos/{TAP_REPOSITORY}/commits/{commit}")["commit"]
    author = result["author"]
    trailer = f"Signed-off-by: {BOT_NAME} <{BOT_EMAIL}>"
    if (not result["verification"]["verified"] or author["name"] != BOT_NAME or author["email"] != BOT_EMAIL
            or trailer not in result["message"].splitlines()):
        raise RuntimeError(f"Published tap commit {commit} failed Verified signature or matching DCO verification")
    print(f"Published verified, DCO-signed tap commit {commit} for {data['tag']}.")


def refresh(*, publish_changes: bool, expected_tag: str = "", expected_commit: str = ""):
    expected = validate_expected(expected_tag, expected_commit)
    head = command(["git", "rev-parse", "HEAD"]).strip()
    remote_head = api(f"repos/{TAP_REPOSITORY}/git/ref/heads/{BRANCH}")["object"]["sha"]
    if COMMIT.fullmatch(head) is None or remote_head != head:
        raise RuntimeError("Tap checkout is stale; rerun refresh against the current distribution branch")
    latest = select_release(published_releases())
    existing = Path("Casks/agent-job-queue.json")
    existing_cask = existing.with_suffix(".rb")
    if existing.exists() != existing_cask.exists():
        raise ValueError("Tap metadata and cask must both exist or both be absent")
    current = validate_manifest(json.loads(existing.read_text())) if existing.exists() else None
    with tempfile.TemporaryDirectory(prefix="agent-queue-tap-") as temporary:
        work = Path(temporary)
        validated = {}
        source_releases = {}

        def checked(tag):
            if tag not in validated:
                source_releases[tag] = release_by_tag(tag)
                validated[tag] = download_release(source_releases[tag], work)
            return validated[tag]

        if expected is not None:
            requested, _ = checked(expected[0])
            if requested["commit"] != expected[1]:
                raise ValueError("Expected commit does not match its public release")
        data, cask = checked(latest["tag_name"])
        if expected is not None:
            if data["build"] < requested["build"]:
                raise ValueError("Latest public release is older than the requested delivery")
            if data["build"] == requested["build"] and data != requested:
                raise ValueError("Requested build has conflicting public release identities")
            verify_successor(requested, data)
        if current is not None:
            published_current, current_cask = checked(current["tag"])
            if current != published_current:
                raise ValueError("Current tap metadata differs from its validated public release")
            if current["build"] > data["build"]:
                raise ValueError("Latest public release would downgrade the tap")
            if current["build"] == data["build"] and current != data:
                raise ValueError("The same build number has conflicting release metadata")
            installed_cask = existing_cask.read_bytes()
            if installed_cask != current_cask.encode():
                historical_cask = download_historical_cask(source_releases[current["tag"]], work)
                if installed_cask != historical_cask:
                    raise ValueError("Current tap cask differs from its historical published release")
            if current["build"] == data["build"]:
                if installed_cask == cask.encode():
                    print("Tap is already current; public metadata, installer and cask verified.")
                    return
                print("Published build verified; template change awaits a newer app release.")
                return
            verify_successor(current, data)
        # The API's expectedHeadOid rejects concurrent owner edits or stale workflow runs.
        if publish_changes:
            publish(data, cask, head)
        else:
            print(f"Validated {data['tag']} and installer checksum; publication disabled.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--publish", action="store_true")
    parser.add_argument("--expected-tag", default="")
    parser.add_argument("--expected-commit", default="")
    args = parser.parse_args()
    validate_expected(args.expected_tag, args.expected_commit)
    if args.publish and not (os.environ.get("GITHUB_ACTIONS") == "true"
                             and os.environ.get("GITHUB_REPOSITORY") == TAP_REPOSITORY
                             and os.environ.get("GITHUB_REF") == f"refs/heads/{BRANCH}"
                             and os.environ.get("GITHUB_EVENT_NAME") in {"schedule", "workflow_dispatch"}):
        raise ValueError("Automatic publication is restricted to tap schedule or dispatch workflows")
    refresh(publish_changes=args.publish, expected_tag=args.expected_tag, expected_commit=args.expected_commit)


if __name__ == "__main__":
    main()
