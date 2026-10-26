#!/bin/bash
# Inert lifecycle tests. Never registers a service or terminates a real app.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

# Both distributed architectures must compile against the macOS 15 API floor.
for architecture in arm64 x86_64; do
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -Onone \
    -target "$architecture-apple-macos15.0" -framework Foundation -framework AppKit -framework ServiceManagement \
    "$root/native/AgentServices.swift" "$root/native/tests/ServiceManagementTests.swift" \
    -o "$temporary/ServiceManagementTests-$architecture"
done
case "$(uname -m)" in
  arm64|x86_64) "$temporary/ServiceManagementTests-$(uname -m)" ;;
  *) echo "Unsupported test architecture" >&2; exit 1 ;;
esac
