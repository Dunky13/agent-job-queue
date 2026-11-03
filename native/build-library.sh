#!/bin/bash
# Compile the app's code, every native/*.swift, as the one library the app and tests link.
# release is what ships. test adds internal access for @testable and the hooks in tests/hooks.
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "$0")" && pwd)
output=${1:?Usage: native/build-library.sh OUTPUT_DIRECTORY ARCHITECTURE release|test}
architecture=${2:?Usage: native/build-library.sh OUTPUT_DIRECTORY ARCHITECTURE release|test}
sources=("$source_dir"/*.swift)
case "${3:-}" in
  release) flags=(-O) ;;
  test)
    flags=(-Onone -enable-testing -D AGENT_QUEUE_TEST_HOOKS)
    sources+=("$source_dir"/tests/hooks/*.swift)
    ;;
  *) echo "Usage: native/build-library.sh OUTPUT_DIRECTORY ARCHITECTURE release|test" >&2; exit 2 ;;
esac
mkdir -p -- "$output"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library "${flags[@]}" \
  -target "$architecture-apple-macos15.0" -module-name AgentQueueCore \
  -emit-module -emit-module-path "$output/AgentQueueCore.swiftmodule" \
  -emit-library -static -o "$output/libAgentQueueCore.a" "${sources[@]}"
