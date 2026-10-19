#!/bin/bash
# Produce a self-contained native installer. End users do not need Xcode.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
version=${VERSION:-$(cat "$root/VERSION")}
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || { echo 'Invalid VERSION.' >&2; exit 1; }
destination=${DIST_DIR:-"$root/dist"}
mkdir -p "$destination"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
installer="$work/Agent Job Queue Installer.app"
payload="$installer/Contents/Resources/payload"
app="$payload/Agent Queue.app"
mkdir -p "$installer/Contents/MacOS" "$app/Contents/MacOS" "$payload/source/remote" "$payload/source/scripts" "$payload/source/bin"
ARCHS='arm64 x86_64' "$root/native/build.sh" "$app/Contents/MacOS/AgentQueue"
for architecture in arm64 x86_64; do
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -O -target "$architecture-apple-macos15.0" \
    -framework SwiftUI -framework AppKit "$root/native/Installer.swift" -o "$work/Installer-$architecture"
done
xcrun lipo -create "$work/Installer-arm64" "$work/Installer-x86_64" -output "$installer/Contents/MacOS/Installer"
cp "$root/agent_heavy.py" "$root/install-remote.sh" "$payload/source/"
cp "$root/remote/control.py" "$root/remote/server.py" "$root/remote/requirements.txt" "$root/remote/requirements.lock" "$payload/source/remote/"
cp -R "$root/remote/static" "$payload/source/remote/"
cp "$root/scripts/install-user.sh" "$root/scripts/migrate-installation.py" "$payload/source/scripts/"
cp "$root/bin/agent-heavy" "$root/bin/agent-remote" "$payload/source/bin/"
for document in README.md LICENSE THIRD-PARTY-NOTICES.md VERSION; do
  [[ ! -f "$root/$document" ]] || cp "$root/$document" "$payload/source/$document"
done
# Ship only runtime web files, never tests/build entries or local state.
rm -f "$payload/source/remote/static/schema.test.mjs" "$payload/source/remote/static/vendor/entry.mjs"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>AgentQueue</string>
<key>CFBundleIdentifier</key><string>io.went.agent-queue</string>
<key>CFBundleName</key><string>Agent Queue</string>
<key>CFBundleDisplayName</key><string>Agent Queue</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$version</string>
<key>CFBundleVersion</key><string>$version</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
cat > "$installer/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>Installer</string>
<key>CFBundleIdentifier</key><string>io.went.agent-queue.installer</string>
<key>CFBundleName</key><string>Agent Job Queue Installer</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$version</string>
<key>CFBundleVersion</key><string>$version</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
plutil -lint "$app/Contents/Info.plist" "$installer/Contents/Info.plist"
codesign --force --sign - "$app"
codesign --force --sign - "$installer"
codesign --verify --strict --deep "$installer"
base="Agent-Job-Queue-Installer-$version-universal"
ditto -c -k --sequesterRsrc --keepParent "$installer" "$destination/$base.zip"
if [[ ${BUILD_DMG:-1} == 1 ]]; then
  hdiutil create -volname 'Agent Job Queue' -srcfolder "$installer" -ov -format UDZO "$destination/$base.dmg"
fi
(cd "$destination" && shasum -a 256 "$base.zip") > "$destination/$base.zip.sha256"
if [[ ${BUILD_DMG:-1} == 1 ]]; then
  (cd "$destination" && shasum -a 256 "$base.dmg") > "$destination/$base.dmg.sha256"
fi
printf 'Built %s\n' "$destination/$base.zip"
