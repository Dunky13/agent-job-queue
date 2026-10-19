#!/bin/zsh
set -eu
source_dir=${0:A:h}
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
# Keep the real store and views; replace only the app entry point with the test entry point.
sed '/^@main$/d' "$source_dir/../AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit \
  "$temporary/AgentQueue.swift" "$source_dir/../AgentIntegration.swift" "$source_dir/../AgentUpdates.swift" "$source_dir/../AgentLearning.swift" "$source_dir/RefreshTests.swift" -o "$temporary/RefreshTests"
"$temporary/RefreshTests"
