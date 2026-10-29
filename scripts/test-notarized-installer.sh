#!/bin/bash
# Verify final distribution bytes and the production app-copy boundary.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/macos-signing.sh
source "$root/scripts/macos-signing.sh"
if [[ $# != 3 ]]; then
  printf 'Usage: %s APP.zip APP.dmg EXPECTED_TEAM_ID\n' "$0" >&2
  exit 2
fi
archive=$1
image=$2
team=$3
[[ "$team" =~ ^[A-Z0-9]{10}$ ]] || { printf 'Invalid Apple team ID.\n' >&2; exit 2; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
mkdir "$work/extracted" "$work/installed"
ditto -x -k "$archive" "$work/extracted"
app="$work/extracted/Agent Queue.app"
"$root/scripts/macos-signing.sh" verify-app "$app" "$team"
(cd "$root" && go run -p 2 ./cmd/queue-tool verify-app --app "$app")
# Finder preserves quarantine when a user drags the app out of a downloaded
# archive or image. Keep it in place throughout copying and policy assessment.
xattr -w com.apple.quarantine '0081;00000000;AgentQueueSigningTest;' "$app"
ditto --rsrc --extattr --qtn --acl "$app" "$work/installed/Agent Queue.app"
copied="$work/installed/Agent Queue.app"
xattr -p com.apple.quarantine "$copied" >/dev/null
macos_signing_verification_configure "$team"
macos_verify_app_distribution "$copied"
"$root/scripts/macos-signing.sh" verify-dmg "$image" "$team"
bash "$root/scripts/test-dmg.sh" "$image"
printf 'Notarized ZIP, mounted DMG and quarantine-preserving app copy verified.\n'
