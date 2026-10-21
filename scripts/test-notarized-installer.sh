#!/bin/bash
# Verify final distribution bytes and the production app-copy boundary.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
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
xcrun stapler validate "$copied"
syspolicy_check distribution "$copied" --verbose
"$root/scripts/macos-signing.sh" verify-dmg "$image" "$team"
bash "$root/scripts/test-dmg.sh" "$image"
printf 'Notarized ZIP, mounted DMG and quarantine-preserving production app copy verified.\n'
