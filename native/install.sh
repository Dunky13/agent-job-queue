#!/bin/bash
# Developer entry point; users drag the app from the disk image instead.
# Setup runs when the app first opens.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
"$root/scripts/build-app.sh"
version=${VERSION:-$(cat "$root/VERSION")}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ditto -x -k "${DIST_DIR:-$root/dist}/Agent-Queue-$version-universal.zip" "$work"
mkdir -p "$HOME/Applications"
rm -rf "$HOME/Applications/Agent Queue.app"
ditto "$work/Agent Queue.app" "$HOME/Applications/Agent Queue.app"
open "$HOME/Applications/Agent Queue.app"
