#!/usr/bin/env python3
"""Render trusted Homebrew metadata from strictly validated immutable build values."""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

# In this source tree the validator is at the repository root. In the dedicated
# tap it is copied beside this script, so normal sibling imports remain valid.
sys.path.append(str(Path(__file__).resolve().parent.parent.parent))

from release_metadata import FIELDS as FIELDS, REPOSITORY as REPOSITORY, validate_manifest as validate_manifest


def render_cask(data: object) -> str:
    item = validate_manifest(data)
    return f'''cask "agent-job-queue" do
  version "{item['version']},{item['build']}"
  sha256 "{item['sha256']}"

  url "https://github.com/{REPOSITORY}/releases/download/{item['tag']}/{item['asset']}"
  name "Agent Job Queue"
  desc "Shared heavy-command queue with a macOS menu-bar app"
  homepage "https://github.com/{REPOSITORY}"

  depends_on formula: "caddy"
  depends_on formula: "pueue"
  depends_on formula: "python@3.12"
  depends_on formula: "uv"
  depends_on macos: :sequoia

  # Ad hoc signed and not notarized: clear quarantine only on this staged build
  # before executing its installer or copying its app and command wrappers.
  if respond_to?(:preflight_steps)
    preflight_steps do
      run "/usr/bin/xattr",
          args: ["-dr", "com.apple.quarantine", "{{{{staged_path}}}}"],
          must_succeed: true
    end
  else
    preflight do
      system_command "/usr/bin/xattr",
                     args: ["-dr", "com.apple.quarantine", staged_path],
                     must_succeed: true
    end
  end

  # The vendor installer also prepares private per-user CLI support and services.
  payload = "#{{staged_path}}/Agent Job Queue Installer.app/Contents/Resources/payload"
  installer script: {{
    executable: "#{{payload}}/source/scripts/install-user.sh",
    args:       ["--payload", payload],
    sudo:       false,
  }}

  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/bin/agent-heavy"
  binary "Agent Job Queue Installer.app/Contents/Resources/payload/source/bin/agent-remote"

  # Keep the independent backend available to running jobs and other installers.
  uninstall quit:   "io.went.agent-queue",
            delete: [
              "#{{Dir.home}}/Applications/Agent Queue.app",
              "#{{Dir.home}}/Library/LaunchAgents/io.went.agent-queue.plist",
            ]

  caveats <<~EOS
    Installs and opens Agent Queue for this macOS account without a manual setup step.
    Phone access is off until enabled in the app. This build is ad hoc signed, not notarized.
    Installation clears quarantine only on this cask's staged files so they can run.
    Uninstall removes the menu app and Homebrew command links. The shared per-user backend,
    operator commands, queue jobs, credentials and enabled remote access remain available.
    Disable phone access in settings before uninstall if you want its HTTPS service stopped.
  EOS
end
'''


def create_manifest(archive: Path, *, version: str, tag: str, commit: str, build: int) -> dict:
    with archive.open("rb") as handle:
        checksum = hashlib.file_digest(handle, "sha256").hexdigest()
    return validate_manifest({"schemaVersion": 1, "repository": REPOSITORY, "tag": tag, "commit": commit,
                              "version": version, "build": build, "asset": archive.name,
                              "sha256": checksum})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--build", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    data = create_manifest(args.archive, version=args.version, tag=args.tag, commit=args.commit, build=args.build)
    args.output.mkdir(parents=True, exist_ok=True)
    (args.output / "agent-job-queue.json").write_text(json.dumps(data, indent=2) + "\n")
    (args.output / "agent-job-queue.rb").write_text(render_cask(data))


if __name__ == "__main__":
    main()
