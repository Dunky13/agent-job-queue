#!/bin/bash
# Record the selected native tools before importing any Apple credentials.
set -euo pipefail
umask 077
[[ $# == 1 && ! -e $1 && ! -L $1 ]] || { echo 'A new toolchain receipt path is required.' >&2; exit 1; }
xcode=$(xcodebuild -version)
version=$(awk '/^Xcode / {print $2}' <<< "$xcode")
build=$(awk '/^Build version / {print $3}' <<< "$xcode")
[[ -n $version && -n $build ]]
if [[ -n ${EXPECTED_XCODE_VERSION:-} && $version != "$EXPECTED_XCODE_VERSION" ]] ||
  [[ -n ${EXPECTED_XCODE_BUILD:-} && $build != "$EXPECTED_XCODE_BUILD" ]]; then
  printf 'Apple toolchain selection failed: Xcode=%s build=%s.\n' "$version" "$build" >&2
  exit 1
fi
# Resolve tools now so a missing component fails before credential setup.
for tool in swift clang lipo notarytool stapler; do xcrun --find "$tool" >/dev/null; done
sdk=$(xcrun --sdk macosx --show-sdk-version)
swift=$(xcrun swift --version)
go_version=$(go version)
notarytool=$(xcrun notarytool --version)
os=$(sw_vers -productVersion)
architecture=$(uname -m)
[[ -n $sdk && -n $swift && -n $go_version && -n $notarytool && -n $os && -n $architecture ]] || {
  printf '%s\n' 'A required Apple toolchain probe returned no identity.' >&2
  exit 1
}
jq -n --arg xcode "$version" --arg build "$build" \
  --arg sdk "$sdk" --arg swift "$swift" --arg go "$go_version" --arg notarytool "$notarytool" \
  --arg os "$os" --arg arch "$architecture" \
  --arg image "${ImageOS:-local}" --arg image_version "${ImageVersion:-local}" \
  --arg source "${BUILD_SHA:-${GITHUB_SHA:-local}}" --arg automation "${GITHUB_SHA:-local}" '
  {schema:1, xcode:$xcode, xcode_build:$build, macos_sdk:$sdk, swift:$swift,
   go:$go, notarytool:$notarytool, os:$os, host_architecture:$arch,
   runner_image:$image, runner_image_version:$image_version, source_sha:$source, automation_sha:$automation}
' > "$1"
cat "$1"
