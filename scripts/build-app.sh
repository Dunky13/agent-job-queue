#!/bin/bash
# Produce the self-contained drag-and-drop app. End users do not need Xcode.
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
app="$work/Agent Queue.app"
source_dir="$app/Contents/Resources/payload/source"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$app/Contents/Library/LaunchAgents" "$source_dir"
ARCHS='arm64 x86_64' "$root/native/build.sh" "$app/Contents/MacOS/AgentQueue"
for architecture in arm64 x86_64; do
  goarch=$architecture
  [[ "$architecture" != x86_64 ]] || goarch=amd64
  for command in agent-heavy agent-remote; do
    (cd "$root" && GOOS=darwin GOARCH="$goarch" CGO_ENABLED=1 CC="$(xcrun --find clang)" SDKROOT="$sdk" \
      CGO_CFLAGS="-arch $architecture -isysroot \"$sdk\" -mmacosx-version-min=15.0" \
      CGO_LDFLAGS="-arch $architecture -isysroot \"$sdk\" -mmacosx-version-min=15.0" \
      go build -p 2 -trimpath -ldflags='-s -w' -o "$work/$command-$architecture" "./cmd/$command")
  done
done
# Helpers run from the bundle itself: launchd starts them through BundleProgram
# and the CLI wrappers exec them in place. Nothing is copied out at setup.
for command in agent-heavy agent-remote; do
  xcrun lipo -create "$work/$command-arm64" "$work/$command-x86_64" -output "$app/Contents/MacOS/$command"
done
cp "$root/native/Resources/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
cp "$work/build-info.json" "$app/Contents/Resources/build-info.json"
cp "$work/build-info.json" "$source_dir/build-info.json"
(cd "$root" && go run -p 2 ./cmd/queue-tool licenses --output "$source_dir/licenses")
for document in README.md LICENSE THIRD-PARTY-NOTICES.md; do
  [[ ! -f "$root/$document" ]] || cp "$root/$document" "$source_dir/$document"
done
printf '%s\n' "$version" > "$source_dir/VERSION"
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
cp "$root"/native/LaunchAgents/*.plist "$app/Contents/Library/LaunchAgents/"
plutil -lint "$app/Contents/Info.plist" "$app"/Contents/Library/LaunchAgents/*.plist
macos_package_app "$app" "$destination" "$version"
