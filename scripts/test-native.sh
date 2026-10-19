#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
zsh "$root/native/tests/run-refresh-tests.sh"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
sed '/^@main$/d' "$root/native/AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit \
  "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/tests/BehaviorTests.swift" -o "$temporary/BehaviorTests"
"$temporary/BehaviorTests" --fixture "$root/native/tests/fixtures/queue.json" --verify-confirmation-navigation
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit \
  "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/tests/UpdateTests.swift" -o "$temporary/UpdateTests"
"$temporary/UpdateTests"
