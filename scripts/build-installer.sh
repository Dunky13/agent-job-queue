#!/bin/bash
# Produce a self-contained native installer. End users do not need Xcode.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/macos-signing.sh
source "$root/scripts/macos-signing.sh"
# Reject partial production configuration before starting any compilation.
macos_signing_configure
version=${VERSION:-$(cat "$root/VERSION")}
destination=${DIST_DIR:-"$root/dist"}
mkdir -p "$destination"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sdk=$(xcrun --sdk macosx --show-sdk-path)
(cd "$root" && go run -p 2 ./cmd/queue-build-info --version "$version" --output "$work/build-info.json")
bundle_build=$(cd "$root" && go run -p 2 ./cmd/queue-build-info --version "$version" --field build)
installer="$work/Agent Job Queue Installer.app"
payload="$installer/Contents/Resources/payload"
app="$payload/Agent Queue.app"
mkdir -p "$installer/Contents/MacOS" "$app/Contents/MacOS" "$app/Contents/Resources" "$payload/source/scripts" "$payload/source/bin" "$payload/source/links"
ARCHS='arm64 x86_64' "$root/native/build.sh" "$app/Contents/MacOS/AgentQueue"
for architecture in arm64 x86_64; do
  xcrun swiftc -warnings-as-errors -swift-version 6 -parse-as-library -O -target "$architecture-apple-macos15.0" \
    -framework SwiftUI -framework AppKit -framework ServiceManagement "$root/native/QueueBrand.swift" "$root/native/AgentServices.swift" "$root/native/Installer.swift" -o "$work/Installer-$architecture"
  goarch=$architecture
  [[ "$architecture" != x86_64 ]] || goarch=amd64
  for command in agent-heavy agent-remote; do
    (cd "$root" && GOOS=darwin GOARCH="$goarch" CGO_ENABLED=1 CC="$(xcrun --find clang)" SDKROOT="$sdk" \
      CGO_CFLAGS="-arch $architecture -isysroot \"$sdk\" -mmacosx-version-min=15.0" \
      CGO_LDFLAGS="-arch $architecture -isysroot \"$sdk\" -mmacosx-version-min=15.0" \
      go build -p 2 -trimpath -ldflags='-s -w' -o "$work/$command-$architecture" "./cmd/$command")
  done
done
xcrun lipo -create "$work/Installer-arm64" "$work/Installer-x86_64" -output "$installer/Contents/MacOS/Installer"
for command in agent-heavy agent-remote; do
  xcrun lipo -create "$work/$command-arm64" "$work/$command-x86_64" -output "$payload/source/bin/$command"
  cp "$root/packaging/links/$command" "$payload/source/links/$command"
done
cp "$root/native/Resources/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
cp "$root/native/Resources/AppIcon.icns" "$installer/Contents/Resources/AppIcon.icns"
cp "$work/build-info.json" "$app/Contents/Resources/build-info.json"
cp "$work/build-info.json" "$installer/Contents/Resources/build-info.json"
cp "$work/build-info.json" "$payload/source/build-info.json"
cp "$root/scripts/install-user.sh" "$payload/source/scripts/"
(cd "$root" && go run -p 2 ./cmd/queue-tool licenses --output "$payload/source/licenses")
for document in README.md LICENSE THIRD-PARTY-NOTICES.md VERSION; do
  [[ ! -f "$root/$document" ]] || cp "$root/$document" "$payload/source/$document"
done
printf '%s\n' "$version" > "$payload/source/VERSION"
# Dashboard assets are embedded in the Go remote command.
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>AgentQueue</string>
<key>CFBundleIdentifier</key><string>io.went.agent-queue</string>
<key>CFBundleName</key><string>Agent Queue</string>
<key>CFBundleDisplayName</key><string>Agent Queue</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundleShortVersionString</key><string>$version</string>
<key>CFBundleVersion</key><string>$bundle_build</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
<key>AgentQueueEmbeddedPayload</key><true/>
<key>AgentQueueServiceManagement</key><true/>
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
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundleShortVersionString</key><string>$version</string>
<key>CFBundleVersion</key><string>$bundle_build</string>
<key>LSMinimumSystemVersion</key><string>15.0</string>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
plutil -lint "$app/Contents/Info.plist" "$installer/Contents/Info.plist"
# The app carries support metadata; its only helper copies live in MacOS.
# Copy only source support, never the app or its enclosing installer.
mkdir -p "$app/Contents/Resources/payload"
ditto "$payload/source" "$app/Contents/Resources/payload/source"
for command in agent-heavy agent-remote; do
  mv "$app/Contents/Resources/payload/source/bin/$command" "$app/Contents/MacOS/$command"
done
rmdir "$app/Contents/Resources/payload/source/bin"
mkdir -p "$app/Contents/Library/LaunchAgents"
cp "$root"/native/LaunchAgents/*.plist "$app/Contents/Library/LaunchAgents/"
plutil -lint "$app"/Contents/Library/LaunchAgents/*.plist
macos_package_installer "$installer" "$destination" "$version"
