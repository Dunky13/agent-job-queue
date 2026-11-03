#!/bin/bash
# Only injected controllers and public jobs are used; no live queue or setup runs.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
[[ $# == 1 || ( $# == 3 && $2 == --before-ref && $3 =~ ^[0-9a-f]{40}$ ) ]] || {
  printf 'Usage: %s OUTPUT_DIRECTORY [--before-ref COMMIT_SHA]\n' "$0" >&2
  exit 1
}
mkdir -p "$1"
output=$(cd -- "$1" && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

# The historical ref predates the library, so only it still compiles from rewritten source.
build_before_fixture() {
  local source=$1
  local queue_source="$source/AgentQueue.swift"
  # Supply the same injected-provider argument seam to the historical store.
  # This changes no baseline view code and keeps fixture badges/controls equal.
  awk '
    /^final class QueueStore:/ { store=1 }
    store && /^    init\(statusLoader:/ { initializer=1 }
    initializer && /^         \}\) \{/ {
      sub(/\}\) \{/, "}, arguments: [String] = CommandLine.arguments) {")
    }
    initializer && /let args = CommandLine.arguments/ {
      sub(/let args = CommandLine.arguments/, "let args = arguments")
      initializer=0
    }
    { print }
  ' "$queue_source" > "$temporary/before-injected.swift"
  # The historical store injects argv where the current one injects a typed command,
  # and it compiles into one module with the test rather than linking a library.
  sed -e 's/\(backend\.command(.0\)\.arguments)/\1)/' -e '/^@testable import AgentQueueCore$/d' \
    "$root/native/tests/ResumeUITests.swift" > "$temporary/before-ResumeUITests.swift"
  sed '/^@main$/d' "$temporary/before-injected.swift" > "$temporary/before-AgentQueue.swift"
  local sources=("$source/QueueBrand.swift" "$temporary/before-AgentQueue.swift"
    "$source/AgentIntegration.swift" "$source/AgentUpdates.swift" "$source/AgentLearning.swift"
    "$source/AgentBootstrap.swift" "$source/AgentServices.swift")
  if [[ -f "$source/AgentSupervisor.swift" ]]; then sources+=("$source/AgentSupervisor.swift"); fi
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone \
    -target "$(uname -m)-apple-macos15.0" -framework SwiftUI -framework AppKit -framework ServiceManagement \
    "${sources[@]}" "$temporary/before-ResumeUITests.swift" -o "$temporary/before-tests"
}

if [[ $# == 3 ]]; then
  mkdir -p "$temporary/before"
  git -C "$root" archive "$3" native | tar -x -C "$temporary/before"
  build_before_fixture "$temporary/before/native"
  "$temporary/before-tests" --fixture "$root/native/tests/fixtures/queue.json" \
    --input "$root/native/tests/fixtures/queue.json" --output "$output/before" --capture-only
fi
bash "$root/native/tests/build-test.sh" "$temporary/after-tests" "$root/native/tests/ResumeUITests.swift"
"$temporary/after-tests" --fixture "$root/native/tests/fixtures/queue.json" \
  --input "$root/native/tests/fixtures/queue.json" --output "$output/after"
[[ "$(plutil -extract passed raw -o - "$output/after/verification.json")" == true ]]
