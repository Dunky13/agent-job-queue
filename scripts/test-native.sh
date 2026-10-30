#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
zsh "$root/native/tests/run-refresh-tests.sh"
bash "$root/native/tests/run-bootstrap-tests.sh"
bash "$root/native/tests/run-service-management-tests.sh"
bash "$root/native/tests/run-supervisor-tests.sh"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
sed '/^@main$/d' "$root/native/AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" "$root/native/AgentSupervisor.swift" "$root/native/tests/BehaviorTests.swift" -o "$temporary/BehaviorTests"
"$temporary/BehaviorTests" --fixture "$root/native/tests/fixtures/queue.json" --verify-confirmation-navigation
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" "$root/native/AgentSupervisor.swift" "$root/native/tests/UpdateTests.swift" -o "$temporary/UpdateTests"
"$temporary/UpdateTests"

xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" "$root/native/AgentSupervisor.swift" "$root/native/tests/LearningTests.swift" -o "$temporary/LearningTests"
"$temporary/LearningTests"

xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" "$root/native/AgentSupervisor.swift" "$root/native/tests/TransportTests.swift" -o "$temporary/TransportTests"
"$temporary/TransportTests"
