#!/bin/bash
# Verify the signed installer as users see it on the mounted disk image.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
image=$1
work=$(mktemp -d)
mountpoint="$work/image"
mounted=0
cleanup() {
  local result=$?
  if [[ $mounted == 1 ]]; then
    if ! hdiutil detach "$mountpoint"; then
      printf 'Could not detach the test image; retaining %s\n' "$work" >&2
      return 1
    fi
  fi
  rm -rf "$work"
  return "$result"
}
trap cleanup EXIT
mkdir "$mountpoint"
hdiutil verify "$image"
# Attachment can fail after exposing a volume; cleanup must still detach it.
mounted=1
hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$mountpoint" "$image"
installer="$mountpoint/Agent Job Queue Installer.app"
for executable in \
  'Contents/MacOS/Installer' \
  'Contents/Resources/payload/Agent Queue.app/Contents/MacOS/AgentQueue' \
  'Contents/Resources/payload/source/bin/agent-heavy' \
  'Contents/Resources/payload/source/bin/agent-remote'; do
  if [[ ! -x "$installer/$executable" ]]; then
    printf 'Disk image executable is missing or not executable: %s\n' "$executable" >&2
    exit 1
  fi
done
(cd "$root" && go run -p 2 ./cmd/queue-tool verify-installer \
  --installer "$installer")
