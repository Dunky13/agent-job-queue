#!/bin/bash
# Verify the drag-to-Applications disk image layout and the app it carries.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# --finder-layout also requires the committed Finder layout. Only freshly built
# images use it: release recovery re-verifies images built before the layout
# existed or changed, and those must stay verifiable.
if [[ $# == 2 && $2 == --finder-layout ]]; then
  finder_layout=1
elif [[ $# == 1 ]]; then
  finder_layout=0
else
  printf 'Usage: test-dmg.sh IMAGE.dmg [--finder-layout]\n' >&2
  exit 2
fi
[[ -n $1 ]] || { printf 'Usage: test-dmg.sh IMAGE.dmg [--finder-layout]\n' >&2; exit 2; }
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
applications="$mountpoint/Applications"
if [[ ! -d $app || -L $app ]]; then
  printf 'Disk image has a missing or malformed app.\n' >&2
  exit 1
fi
if [[ ! -L $applications ]] || ! target=$(readlink -n "$applications" && printf '.') || [[ $target != /Applications. ]]; then
  printf 'Drag disk image requires Applications -> /Applications.\n' >&2
  exit 1
fi
if [[ $finder_layout == 1 ]] && ! cmp -s "$mountpoint/.DS_Store" "$root/packaging/dmg/DS_Store"; then
  printf 'Disk image lacks the committed Finder layout.\n' >&2
  exit 1
fi
# Nothing but the app and the link may be offered to the user. Volume
# bookkeeping that macOS adds on mount is not part of the image contents.
shopt -s nullglob dotglob
for entry in "$mountpoint"/*; do
  case "${entry##*/}" in
    'Agent Queue.app'|Applications) ;;
    .fseventsd|.Trashes|.Spotlight-V100|.DS_Store|.VolumeIcon.icns|.metadata_never_index|.TemporaryItems) ;;
    *) printf 'Unexpected disk image entry: %s\n' "${entry##*/}" >&2; exit 1 ;;
  esac
done
for executable in Contents/MacOS/AgentQueue Contents/MacOS/agent-heavy Contents/MacOS/agent-remote; do
  if [[ ! -f "$app/$executable" || -L "$app/$executable" || ! -x "$app/$executable" ]]; then
    printf 'Disk image executable is missing or not executable: %s\n' "$executable" >&2
    exit 1
  fi
done
(cd "$root" && go run -p 2 ./cmd/queue-tool verify-app --app "$app")
