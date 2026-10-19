#!/usr/bin/env python3
"""Refresh the generated tap from validated public app release metadata."""
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


def select_release(releases):
    if not isinstance(releases, list):
        raise ValueError("Unexpected source release response")
    candidates = [release for release in releases if isinstance(release, dict) and not release.get("draft")
                  and isinstance(release.get("tag_name"), str) and TAG.fullmatch(release["tag_name"])
                  and any(asset.get("name") == "agent-job-queue.json" for asset in release.get("assets", [])
                          if isinstance(asset, dict))]
    return max(candidates, key=lambda release: int(TAG.fullmatch(release["tag_name"])[1]), default=None)


def verify_release(manifest, release, archive: Path):
    data = validate_manifest(manifest)
    if data["tag"] != release["tag_name"] or data["commit"] != release["target_commitish"]:
        raise ValueError("Release identity does not match manifest")
    assets = {asset["name"] for asset in release.get("assets", []) if isinstance(asset, dict) and "name" in asset}
    if data["asset"] not in assets:
        raise ValueError("Release does not contain the expected installer")
    with archive.open("rb") as handle:
        checksum = hashlib.file_digest(handle, "sha256").hexdigest()
    if checksum != data["sha256"]:
        raise ValueError("Downloaded installer checksum does not match manifest")
    return data


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
    result = api(f"repos/{TAP_REPOSITORY}/commits/{commit}")["commit"]
    author = result["author"]
    trailer = f"Signed-off-by: {author['name']} <{author['email']}>"
    if not result["verification"]["verified"] or trailer not in result["message"].splitlines():
        raise RuntimeError(f"Published tap commit {commit} failed Verified signature or matching DCO verification")
    print(f"Published verified, DCO-signed tap commit {commit} for {data['tag']}.")


def refresh(*, publish_changes: bool):
    head = command(["git", "rev-parse", "HEAD"]).strip()
    remote_head = api(f"repos/{TAP_REPOSITORY}/git/ref/heads/{BRANCH}")["object"]["sha"]
    if not re.fullmatch(r"[0-9a-f]{40}", head) or remote_head != head:
        raise RuntimeError("Tap checkout is stale; rerun refresh against the current distribution branch")
    release = select_release(api(f"repos/{REPOSITORY}/releases?per_page=30"))
    if release is None:
        print("No published app build with Homebrew metadata exists yet; tap unchanged.")
        return
    existing = Path("Casks/agent-job-queue.json")
    current = validate_manifest(json.loads(existing.read_text())) if existing.exists() else None
    if current is not None and current["build"] >= int(TAG.fullmatch(release["tag_name"])[1]):
        print("Tap is already current; no commit created.")
        return
    with tempfile.TemporaryDirectory(prefix="agent-queue-tap-") as temporary:
        work = Path(temporary)
        tag = release["tag_name"]
        command(["gh", "release", "download", tag, "--repo", REPOSITORY, "--pattern", "agent-job-queue.json", "--dir", str(work)])
        manifest_file = work / "agent-job-queue.json"
        if manifest_file.stat().st_size > 16_384:
            raise ValueError("Release manifest exceeds its size limit")
        manifest = validate_manifest(json.loads(manifest_file.read_text()))
        command(["gh", "release", "download", tag, "--repo", REPOSITORY, "--pattern", manifest["asset"], "--dir", str(work)])
        data = verify_release(manifest, release, work / manifest["asset"])
        cask = render_cask(data)
        cask_path = work / "agent-job-queue.rb"
        cask_path.write_text(cask)
        command(["brew", "ruby", str(Path(__file__).with_name("test-cask.rb")), str(cask_path)])
        # The API's expectedHeadOid rejects concurrent owner edits or stale workflow runs.
        if publish_changes:
            publish(data, cask, head)
        else:
            print(f"Validated {data['tag']} and installer checksum; publication disabled.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--publish", action="store_true")
    args = parser.parse_args()
    if args.publish and os.environ.get("GITHUB_REPOSITORY") != TAP_REPOSITORY:
        raise ValueError("Automatic publication is restricted to the dedicated tap repository")
    refresh(publish_changes=args.publish)


if __name__ == "__main__":
    main()
