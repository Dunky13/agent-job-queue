#!/bin/bash
# Run only simulated jobs in a separate graphical fixture executable.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
report=${1:?Usage: native/tests/run-menu-bar-ui-tests.sh OUTPUT.json [--require-foreground]}
shift
report="$(cd -- "$(dirname -- "$report")" && pwd)/$(basename -- "$report")"
temporary=$(mktemp -d)
fixture_app="$temporary/MenuBarFixture.app"
fixture_binary="$fixture_app/Contents/MacOS/MenuBarUITests"
fixture_pid_file="$temporary/fixture.pid"
fixture_launch_pid=""
cleanup() {
  if [[ -n "$fixture_launch_pid" ]]; then
    kill -TERM "$fixture_launch_pid" 2>/dev/null || true
    wait "$fixture_launch_pid" 2>/dev/null || true
  fi
  if [[ -f "$fixture_pid_file" ]]; then
    local fixture_pid
    read -r fixture_pid < "$fixture_pid_file"
    if [[ "$fixture_pid" =~ ^[0-9]+$ ]] && [[ "$(ps -ww -p "$fixture_pid" -o comm=)" == "$fixture_binary" ]]; then
      kill -TERM "$fixture_pid" 2>/dev/null || true
    fi
  fi
  rm -rf "$temporary"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$fixture_app/Contents/MacOS"
cat > "$fixture_app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>MenuBarUITests</string>
  <key>CFBundleIdentifier</key><string>io.went.agentqueue.menu-bar-fixture</string>
  <key>CFBundleName</key><string>Agent Queue Fixture</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>15.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
/usr/libexec/PlistBuddy -c "Set :CFBundleIdentifier io.went.agentqueue.fixture.$(uuidgen)" "$fixture_app/Contents/Info.plist"
sed '/^@main$/d' "$root/native/AgentQueue.swift" > "$temporary/AgentQueue.swift"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone -framework SwiftUI -framework AppKit -framework ServiceManagement \
  "$root/native/QueueBrand.swift" "$temporary/AgentQueue.swift" "$root/native/AgentIntegration.swift" "$root/native/AgentUpdates.swift" \
  "$root/native/AgentLearning.swift" "$root/native/AgentBootstrap.swift" "$root/native/AgentServices.swift" "$root/native/tests/MenuBarUITests.swift" -o "$fixture_binary"
rm -f -- "$report"
open -W -n --stdout "$temporary/stdout.log" --stderr "$temporary/stderr.log" "$fixture_app" \
  --args --fixture "$root/native/tests/fixtures/queue.json" --report "$report" --pid-file "$fixture_pid_file" "$@" &
fixture_launch_pid=$!
for ((attempt = 0; attempt < 90; attempt++)); do
  if ! kill -0 "$fixture_launch_pid" 2>/dev/null; then break; fi
  sleep 1
done
if kill -0 "$fixture_launch_pid" 2>/dev/null; then
  echo "Menu-bar fixture launch did not complete within 90 seconds." >&2
  exit 1
fi
if ! wait "$fixture_launch_pid"; then
  fixture_launch_pid=""
  cat "$temporary/stdout.log" "$temporary/stderr.log" 2>/dev/null || true
  exit 1
fi
fixture_launch_pid=""
cat "$temporary/stdout.log" "$temporary/stderr.log"
[[ "$(plutil -extract passed raw -o - "$report")" == "true" ]]
