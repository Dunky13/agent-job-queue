#!/bin/bash
# Build the menu-bar executable for macOS 15 and later.
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "$0")" && pwd)
output=${1:?Usage: native/build.sh OUTPUT_EXECUTABLE}
architectures=${ARCHS:-"arm64 x86_64"}
mkdir -p -- "$(dirname -- "$output")"
build_dir=$(mktemp -d)
trap 'rm -rf -- "$build_dir"' EXIT
binaries=()
for architecture in $architectures; do
  case "$architecture" in arm64|x86_64) ;; *) echo "Unsupported architecture: $architecture" >&2; exit 1 ;; esac
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -O \
    -target "$architecture-apple-macos15.0" -framework SwiftUI -framework AppKit \
    "$source_dir/AgentQueue.swift" "$source_dir/AgentIntegration.swift" "$source_dir/AgentUpdates.swift" "$source_dir/AgentLearning.swift" -o "$build_dir/$architecture"
  binaries+=("$build_dir/$architecture")
done
xcrun lipo -create "${binaries[@]}" -output "$output"
