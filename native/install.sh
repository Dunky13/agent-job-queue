#!/bin/bash
# Developer entry point; packaged users run the native installer instead.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
"$root/scripts/build-installer.sh"
version=${VERSION:-$(cat "$root/VERSION")}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ditto -x -k "${DIST_DIR:-$root/dist}/Agent-Job-Queue-Installer-$version-universal.zip" "$work"
installer="$work/Agent Job Queue Installer.app"
/bin/bash "$installer/Contents/Resources/payload/source/scripts/install-user.sh" --payload "$installer/Contents/Resources/payload"
