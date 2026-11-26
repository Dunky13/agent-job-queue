#!/bin/bash
# Link Swift sources holding an @main against the test flavour of the app library.
# AGENT_QUEUE_TEST_LIBRARY names an already built library, so a batch compiles the app once.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
output=${1:?Usage: native/tests/build-test.sh OUTPUT_EXECUTABLE SOURCE...}
shift
library=${AGENT_QUEUE_TEST_LIBRARY:-}
if [[ -z "$library" ]]; then
  library=$(mktemp -d)
  trap 'rm -rf -- "$library"' EXIT
  bash "$root/native/build-library.sh" "$library" "$(uname -m)" test
fi
# The library imports Sparkle, so every test binary links it and finds it at its pinned location.
sparkle=${SPARKLE_DIR:-$(bash "$root/packaging/sparkle/fetch.sh")}
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -target "$(uname -m)-apple-macos15.0" \
  -framework SwiftUI -framework AppKit -framework ServiceManagement -F "$sparkle" -framework Sparkle -Xlinker -rpath -Xlinker "$sparkle" \
  -I "$library" -Xlinker -force_load -Xlinker "$library/libAgentQueueCore.a" "$@" -o "$output"
