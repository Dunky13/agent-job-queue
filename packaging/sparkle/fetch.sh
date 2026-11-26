#!/bin/bash
# Download the pinned Sparkle release once, verify it and print the extracted directory.
# Usage: packaging/sparkle/fetch.sh [CACHE_DIRECTORY]
set -euo pipefail
version=2.10.1
sha256=1a856d2a9edb8e628de88121d7be04cf0e2167f8641650b78fca00b14244cf27
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
cache=${1:-${SPARKLE_CACHE:-$root/build/sparkle}}
target="$cache/$version"
if [[ ! -f "$target/.verified" ]]; then
  mkdir -p "$cache"
  archive=$(mktemp "$cache/download.XXXXXX")
  trap 'rm -f -- "$archive"' EXIT
  curl --fail --silent --show-error --location --proto '=https' --max-time 300 \
    "https://github.com/sparkle-project/Sparkle/releases/download/$version/Sparkle-$version.tar.xz" -o "$archive"
  actual=$(shasum -a 256 "$archive" | cut -d' ' -f1)
  [[ $actual == "$sha256" ]] || { echo "Sparkle $version checksum mismatch: $actual" >&2; exit 1; }
  rm -rf -- "$target" && mkdir -p "$target"
  tar -xJf "$archive" -C "$target"
  touch "$target/.verified"
fi
printf '%s\n' "$target"
