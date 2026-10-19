"""Strict public release and installed-build identities shared by packaging and runtime."""
from __future__ import annotations

import argparse
import json
import os
import re
from collections.abc import Mapping
from pathlib import Path

REPOSITORY = "Dunky13/agent-job-queue"
FIELDS = {"schemaVersion", "repository", "tag", "commit", "version", "build", "asset", "sha256"}
BUILD_FIELDS = {"schemaVersion", "repository", "version", "commit", "build"}
MAX_BUILD = 999_999_999
VERSION = re.compile(
    r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-(?:(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)\.)*"
    r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\Z"
)


def validate_build_info(data: object) -> dict:
    """Accept only the fixed repository and explicit, truthful build provenance."""
    if not isinstance(data, dict) or set(data) != BUILD_FIELDS:
        raise ValueError("Unexpected build identity fields")
    if type(data["schemaVersion"]) is not int or data["schemaVersion"] != 1 or data["repository"] != REPOSITORY:
        raise ValueError("Unsupported build identity source")
    if not isinstance(data["version"], str) or not VERSION.fullmatch(data["version"]):
        raise ValueError("Invalid version")
    commit = data["commit"]
    if commit is not None and (not isinstance(commit, str) or not re.fullmatch(r"[0-9a-f]{40}", commit)):
        raise ValueError("Invalid commit")
    build = data["build"]
    if build is not None and (type(build) is not int or not 1 <= build <= MAX_BUILD):
        raise ValueError("Invalid build number")
    if build is not None and commit is None:
        raise ValueError("Tracked builds require a commit")
    return data


def validate_manifest(data: object) -> dict:
    """Validate the immutable manifest before using any release-controlled value."""
    if not isinstance(data, dict) or set(data) != FIELDS:
        raise ValueError("Unexpected Homebrew manifest fields")
    validate_build_info({field: data[field] for field in BUILD_FIELDS})
    if data["build"] is None:
        raise ValueError("Release manifests require a build number")
    if not isinstance(data["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", data["sha256"]):
        raise ValueError("Invalid sha256")
    if data["tag"] != f"build-{data['build']}-{data['commit'][:7]}":
        raise ValueError("Release tag does not match the build and commit")
    if data["asset"] != f"Agent-Job-Queue-Installer-{data['version']}-universal.zip":
        raise ValueError("Unexpected installer asset")
    return data


def generate_build_info(version: str, *, environment: Mapping[str, str] | None = None) -> dict:
    """Identify published main CI or an explicitly supplied build/commit pair.

    A checkout or pull-request build has no release build number. In particular,
    a local git commit alone must never make local artifacts appear up to date.
    """
    source = os.environ if environment is None else environment
    build = source.get("BUILD_NUMBER")
    commit = source.get("SOURCE_COMMIT")
    if (build is None) != (commit is None):
        raise ValueError("BUILD_NUMBER and SOURCE_COMMIT must be supplied together")
    if build is None and (source.get("GITHUB_ACTIONS") == "true"
                          and source.get("GITHUB_REPOSITORY") == REPOSITORY
                          and source.get("GITHUB_EVENT_NAME") == "push"
                          and source.get("GITHUB_REF") == "refs/heads/main"):
        build, commit = source.get("GITHUB_RUN_NUMBER"), source.get("GITHUB_SHA")
        if build is None or commit is None:
            raise ValueError("Published CI build identity is incomplete")
    if build is not None:
        if not isinstance(build, str) or not re.fullmatch(r"[1-9][0-9]{0,8}", build):
            raise ValueError("Invalid build number")
        build = int(build)
    return validate_build_info({"schemaVersion": 1, "repository": REPOSITORY,
                                "version": version, "build": build, "commit": commit})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.write_text(json.dumps(generate_build_info(args.version), indent=2) + "\n")


if __name__ == "__main__":
    main()
