#!/bin/bash
# Usage: bash native/tests/run-bootstrap-ui-tests.sh OUTPUT_DIRECTORY [--build-only]
# Run through the shared heavy queue. Uses AppKit's own events and rendering;
# needs a logged-in graphical macOS session, but no Accessibility permission.
# Output: recovery verification.json, three PNGs and a standalone index.html with
# embedded screenshots; generic-retry/ retains the same checks for other errors.
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
    --open-window --temporary "$temporary" --output "$output"
[[ "$(plutil -extract passed raw -o - "$output/verification.json")" == "true" ]]
mkdir -p -- "$output/generic-retry"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --open-window --temporary "$temporary" --output "$output/generic-retry" --generic-retry
[[ "$(plutil -extract passed raw -o - "$output/generic-retry/verification.json")" == "true" ]]
mkdir -p -- "$output/preflight-summary"
HOME="$temporary/home" CFFIXED_USER_HOME="$temporary/home" \
  "$app/Contents/MacOS/BootstrapUITests" --fixture "$root/native/tests/fixtures/queue.json" \
    --open-window --temporary "$temporary" --output "$output/preflight-summary" --preflight-summary
[[ "$(plutil -extract passed raw -o - "$output/preflight-summary/verification.json")" == "true" ]]
{
  printf '# Bootstrap fixture verification\n\nProduction setup window, injected executor, disposable home and simulated queue.\n\n```json\n'
  cat "$output/verification.json"
  printf '\n```\n\n'
  for picture in 01-progress 02-disable-phone-access 03-normal-fixture-startup; do
    printf '![%s](%s.png)\n\n' "$picture" "$picture"
  done
} > "$output/verification.md"
{
  printf '# Generic retry fixture verification\n\nProduction setup window, injected executor, disposable home and simulated queue.\n\n```json\n'
  cat "$output/generic-retry/verification.json"
  printf '\n```\n\n'
  for picture in 01-progress 02-failure-retry 03-normal-fixture-startup; do
    printf '![%s](%s.png)\n\n' "$picture" "$picture"
  done
} > "$output/generic-retry/verification.md"
{
  printf '# Preflight summary fixture verification\n\nProduction setup window, injected executor, disposable home and simulated queue.\n\n```json\n'
  cat "$output/preflight-summary/verification.json"
  printf '\n```\n\n'
  for picture in 01-preflight-summary 02-waiting 03-wait-stopped 04-normal-fixture-startup; do
    printf '![%s](%s.png)\n\n' "$picture" "$picture"
  done
} > "$output/preflight-summary/verification.md"
