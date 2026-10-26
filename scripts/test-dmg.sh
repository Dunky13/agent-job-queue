#!/bin/bash
# Verify drag-to-Applications and retained legacy installer disk images.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
if [[ $# != 1 || -z $1 ]]; then
  printf 'Usage: test-dmg.sh IMAGE.dmg\n' >&2
  exit 2
fi
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
app="$mountpoint/Agent Queue.app"
installer="$mountpoint/Agent Job Queue Installer.app"
applications="$mountpoint/Applications"
if [[ -d $app && ! -L $app && ! -e $installer && ! -L $installer ]]; then
  if [[ ! -L $applications ]] || ! target=$(readlink -n "$applications" && printf '.') || [[ $target != /Applications. ]]; then
    printf 'Drag disk image requires Applications -> /Applications.\n' >&2
    exit 1
  fi
  bundle=$app
  command=verify-app
  argument=--app
  helper_directory='Contents/Resources/payload/source/bin'
  if plutil -type AgentQueueServiceManagement "$app/Contents/Info.plist" >/dev/null 2>&1; then
    # verify-app checks the marker type and every bundled service definition.
    helper_directory='Contents/MacOS'
  fi
  executables=(
    'Contents/MacOS/AgentQueue'
    "$helper_directory/agent-heavy"
    "$helper_directory/agent-remote"
  )
elif [[ -d $installer && ! -L $installer && ! -e $app && ! -L $app && ! -e $applications && ! -L $applications ]]; then
  bundle=$installer
  command=verify-installer
  argument=--installer
  executables=(
    'Contents/MacOS/Installer'
    'Contents/Resources/payload/Agent Queue.app/Contents/MacOS/AgentQueue'
    'Contents/Resources/payload/source/bin/agent-heavy'
    'Contents/Resources/payload/source/bin/agent-remote'
  )
else
  printf 'Disk image has a missing, mixed, or malformed app layout.\n' >&2
  exit 1
fi
# Do not accept a second app beside the selected distribution layout.
shopt -s nullglob dotglob nocaseglob
for candidate in "$mountpoint/"*.app; do
  if [[ $candidate != "$bundle" ]]; then
    printf 'Unexpected app at disk image root: %s\n' "${candidate##*/}" >&2
    exit 1
  fi
done
for executable in "${executables[@]}"; do
  if [[ ! -f "$bundle/$executable" || -L "$bundle/$executable" || ! -x "$bundle/$executable" ]]; then
    printf 'Disk image executable is missing or not executable: %s\n' "$executable" >&2
    exit 1
  fi
done
(cd "$root" && go run -p 2 ./cmd/queue-tool "$command" "$argument" "$bundle")
