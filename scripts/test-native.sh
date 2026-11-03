#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
# Every suite below links this one build of the app's code.
export AGENT_QUEUE_TEST_LIBRARY="$temporary/library"
bash "$root/native/build-library.sh" "$AGENT_QUEUE_TEST_LIBRARY" "$(uname -m)" test
zsh "$root/native/tests/run-refresh-tests.sh"
bash "$root/native/tests/run-bootstrap-tests.sh"
bash "$root/native/tests/run-service-management-tests.sh"
bash "$root/native/tests/run-supervisor-tests.sh"
bash "$root/native/tests/build-test.sh" "$temporary/BehaviorTests" "$root/native/tests/BehaviorTests.swift"
"$temporary/BehaviorTests" --fixture "$root/native/tests/fixtures/queue.json" --verify-confirmation-navigation
bash "$root/native/tests/build-test.sh" "$temporary/UpdateTests" "$root/native/tests/UpdateTests.swift"
"$temporary/UpdateTests"

bash "$root/native/tests/build-test.sh" "$temporary/LearningTests" "$root/native/tests/LearningTests.swift"
"$temporary/LearningTests"

bash "$root/native/tests/build-test.sh" "$temporary/TransportTests" "$root/native/tests/TransportTests.swift"
"$temporary/TransportTests"
