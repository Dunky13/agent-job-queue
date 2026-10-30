#!/bin/bash
# Usage: bash native/tests/run-bootstrap-ui-tests.sh OUTPUT_DIRECTORY [--build-only]
# Run through the shared heavy queue. Uses AppKit's own events and rendering;
# needs a logged-in graphical macOS session, but no Accessibility permission.
# Output: per-scenario verification.json, cached private AppKit PNGs and a
# standalone index.html. Ordinary startup never requires a setup window.
# Bundle, home, state and preference fixtures are temporary/private.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
output=${1:?Usage: run-bootstrap-ui-tests.sh OUTPUT_DIRECTORY [--build-only]}
shift
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
temporary=$(mktemp -d)
temporary=$(cd -- "$temporary" && pwd -P)
trap 'rm -rf -- "$temporary"' EXIT
app="$temporary/BootstrapFixture.app"
mkdir -p -- "$app/Contents/MacOS" "$temporary/home"
cat > "$app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>BootstrapUITests</string>
<key>CFBundleIdentifier</key><string>io.went.agentqueue.bootstrap-fixture</string>
<key>CFBundleName</key><string>Agent Queue Bootstrap Fixture</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSUIElement</key><true/>
<key>LSMinimumSystemVersion</key><string>15.0</string>
</dict></plist>
PLIST
sed '/^@main$/d' "$root/native/AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" \
  "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" \
  "$root/native/tests/BootstrapFixtures.swift" "$root/native/tests/BootstrapUITests.swift" -o "$app/Contents/MacOS/BootstrapUITests"
if [[ "${1:-}" == "--build-only" ]]; then exit 0; fi
if [[ $# -ne 0 ]]; then echo "Unknown bootstrap UI fixture argument" >&2; exit 2; fi
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output"
[[ "$(plutil -extract passed raw -o - "$output/verification.json")" == "true" ]]
mkdir -p -- "$output/generic-retry"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/generic-retry" --generic-retry
[[ "$(plutil -extract passed raw -o - "$output/generic-retry/verification.json")" == "true" ]]
mkdir -p -- "$output/preflight-summary"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/preflight-summary" --preflight-summary --open-window
[[ "$(plutil -extract passed raw -o - "$output/preflight-summary/verification.json")" == "true" ]]
mkdir -p -- "$output/approved-repair"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/approved-repair" --approved-repair --open-window
[[ "$(plutil -extract passed raw -o - "$output/approved-repair/verification.json")" == "true" ]]
mkdir -p -- "$output/drag-and-drop"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/drag-and-drop" --drag-and-drop
[[ "$(plutil -extract passed raw -o - "$output/drag-and-drop/verification.json")" == "true" ]]
mkdir -p -- "$output/silent-recovery"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/silent-recovery" --silent-recovery
[[ "$(plutil -extract passed raw -o - "$output/silent-recovery/verification.json")" == "true" ]]
mkdir -p -- "$output/cancellable-wait"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/cancellable-wait" --cancellable-wait
[[ "$(plutil -extract passed raw -o - "$output/cancellable-wait/verification.json")" == "true" ]]

for scenario in quit-mutation unknown-requirement; do
  mkdir -p -- "$output/$scenario"
  HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
    "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
      --temporary "$temporary" --output "$output/$scenario" "--$scenario"
  [[ "$(plutil -extract passed raw -o - "$output/$scenario/verification.json")" == "true" ]]
done
mkdir -p -- "$output/direct-quit-wait"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --temporary "$temporary" --output "$output/direct-quit-wait" --cancellable-wait --direct-quit
[[ "$(plutil -extract passed raw -o - "$output/direct-quit-wait/verification.json")" == "true" ]]
