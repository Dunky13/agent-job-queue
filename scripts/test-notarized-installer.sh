#!/bin/bash
# Verify final distribution bytes and the production app-copy boundary.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/macos-signing.sh
source "$root/scripts/macos-signing.sh"
if [[ $# != 3 ]]; then
  printf 'Usage: %s INSTALLER.zip INSTALLER.dmg EXPECTED_TEAM_ID\n' "$0" >&2
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
installer="$work/extracted/Agent Job Queue Installer.app"
"$root/scripts/macos-signing.sh" verify-installer "$installer" "$team"
app="$installer/Contents/Resources/payload/Agent Queue.app"
# Exercise the same ditto options used by the real Go installer's copyApp.
# Quarantine remains in place throughout copying and policy assessment.
xattr -w com.apple.quarantine '0081;00000000;AgentQueueSigningTest;' "$app"
ditto --rsrc --extattr --qtn --acl "$app" "$work/installed/Agent Queue.app"
copied="$work/installed/Agent Queue.app"
xattr -p com.apple.quarantine "$copied" >/dev/null
codesign --verify --strict --deep --all-architectures \
  -R "=anchor apple generic and certificate leaf[subject.OU] = \"$team\" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists" "$copied"
macos_signing_verification_configure "$team"
macos_app_payload_mode "$copied"
if [[ $MACOS_APP_EMBEDDED == 1 ]]; then
  for name in agent-heavy agent-remote; do
    macos_verify_code "$MACOS_APP_HELPERS/$name"
  done
  (cd "$root" && go run -p 2 ./cmd/queue-tool verify-app --app "$copied")
fi
macos_validate_ticket "$copied"
syspolicy_check distribution "$copied" --verbose
"$root/scripts/macos-signing.sh" verify-dmg "$image" "$team"
bash "$root/scripts/test-dmg.sh" "$image"
printf 'Notarized ZIP, mounted DMG and quarantine-preserving production app copy verified.\n'
