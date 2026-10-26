#!/bin/bash
# Headless startup decisions and lifecycle tests with private bundles/homes.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
sed '/^@main$/d' "$root/native/AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" \
  "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" \
  "$root/native/tests/BootstrapFixtures.swift" "$root/native/tests/BootstrapTests.swift" -o "$temporary/BootstrapTests"
"$temporary/BootstrapTests"
