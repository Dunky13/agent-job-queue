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
  library="$build_dir/$architecture-library"
  bash "$source_dir/build-library.sh" "$library" "$architecture" release
  # force_load keeps every object of the app in the binary, as one direct compile did.
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -O \
    -target "$architecture-apple-macos15.0" -framework SwiftUI -framework AppKit -framework ServiceManagement \
    -I "$library" -Xlinker -force_load -Xlinker "$library/libAgentQueueCore.a" \
    "$source_dir/app/AgentQueueMain.swift" -o "$build_dir/$architecture"
  binaries+=("$build_dir/$architecture")
done
xcrun lipo -create "${binaries[@]}" -output "$output"
# Nothing strips this binary, so its symbols show whether test hooks reached it.
symbols=$(xcrun nm -arch all "$output")
grep -q AgentQueueCore <<<"$symbols" || { echo "Could not read app symbols from $output" >&2; exit 1; }
if grep -E 'runSnapshotHooks|FixtureDispatchRecorder|NativeSnapshotRenderer|NavigationVerification' <<<"$symbols" >&2; then
  echo "Test hooks reached the shipped app binary." >&2
  exit 1
fi
