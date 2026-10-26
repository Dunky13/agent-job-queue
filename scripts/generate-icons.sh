#!/bin/bash
# Regenerate committed branding assets using macOS system frameworks only.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT
mkdir -p "$temporary/AppIcon.iconset" "$root/native/Resources"
xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -framework AppKit \
  "$root/native/QueueBrand.swift" "$root/native/tools/GenerateIcons.swift" -o "$temporary/generate-icons"
"$temporary/generate-icons" "$root" "$temporary/AppIcon.iconset"
xcrun iconutil -c icns "$temporary/AppIcon.iconset" -o "$root/native/Resources/AppIcon.icns"
