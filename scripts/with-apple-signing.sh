#!/bin/bash
# Run one finite command with private, temporary Apple signing credentials.
set +x
set -euo pipefail
umask 077

# Reject untrusted hosted jobs before inspecting credentials or invoking tools.
if [[ ${GITHUB_ACTIONS:-} == true ]] &&
  [[ ${GITHUB_EVENT_NAME:-} != push || ${GITHUB_REF:-} != refs/heads/main ]]; then
  printf '%s\n' 'Apple signing requires a trusted push to main.' >&2
  exit 1
fi
if [[ $# == 0 ]]; then
  printf '%s\n' 'Usage: with-apple-signing.sh command [arguments...]' >&2
  exit 1
fi
for variable in APPLE_DEVELOPER_ID_P12_BASE64 APPLE_DEVELOPER_ID_P12_PASSWORD \
  APPLE_NOTARY_KEY_P8 APPLE_SIGNING_IDENTITY APPLE_TEAM_ID \
  APPLE_NOTARY_KEY_ID APPLE_NOTARY_ISSUER_ID; do
  if [[ -z ${!variable:-} ]]; then
    printf 'Apple signing requires %s.\n' "$variable" >&2
    exit 1
  fi
done

work=
keychain=
keychain_created=0
child_pid=
cleanup() {
  local status=$1
  trap - EXIT
  trap '' INT TERM
  if [[ -n $keychain ]] && [[ $keychain_created == 1 || -e $keychain ]]; then
    if ! security delete-keychain "$keychain" >/dev/null 2>&1; then
      printf '%s\n' 'Could not delete the temporary Apple signing keychain.' >&2
      status=1
    fi
  fi
  if [[ -n $work ]] && ! rm -rf -- "$work"; then
    printf '%s\n' 'Could not remove temporary Apple signing credentials.' >&2
    status=1
  fi
  exit "$status"
}
finish() {
  local status=$1
  local deadline
  trap '' INT TERM
  # A signal can arrive between spawning the command and assigning its PID.
  # Bash 3 treats even a defaulted $! as unbound before the first async child.
  set +u
  child_pid=${child_pid:-$!}
  set -u
  if [[ -n $child_pid ]]; then
    # Async commands can inherit SIGINT as ignored, so use TERM for teardown.
    # The command and all its descendants own a separate process group.
    kill -TERM -- "-$child_pid" 2>/dev/null || true
    # Count elapsed time, including process-launch delays on a busy host.
    deadline=$((SECONDS + 5))
    while ((SECONDS < deadline)); do
      if ! kill -0 -- "-$child_pid" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    kill -KILL -- "-$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
  fi
  cleanup "$status"
}
trap 'finish "$?"' EXIT
trap 'finish 130' INT
trap 'finish 143' TERM

work=$(mktemp -d "${TMPDIR:-/tmp}/agent-apple-signing.XXXXXX")
keychain="$work/signing.keychain-db"
certificate="$work/developer-id.p12"
notary_key="$work/notary-key.p8"
if ! printf '%s' "$APPLE_DEVELOPER_ID_P12_BASE64" | base64 -D > "$certificate" 2>/dev/null; then
  printf '%s\n' 'Could not decode the Apple signing certificate.' >&2
  exit 1
fi
printf '%s' "$APPLE_NOTARY_KEY_P8" > "$notary_key"
if [[ ! -s $certificate || ! -s $notary_key ]]; then
  printf '%s\n' 'Apple signing credentials must not be empty.' >&2
  exit 1
fi
if ! keychain_password=$(openssl rand -hex 32 2>/dev/null) || [[ -z $keychain_password ]]; then
  printf '%s\n' 'Could not generate the temporary keychain password.' >&2
  exit 1
fi
if ! security create-keychain -p "$keychain_password" "$keychain" >/dev/null 2>&1; then
  printf '%s\n' 'Could not create the temporary Apple signing keychain.' >&2
  exit 1
fi
keychain_created=1
if ! security unlock-keychain -p "$keychain_password" "$keychain" >/dev/null 2>&1 ||
  ! security set-keychain-settings -lut 21600 "$keychain" >/dev/null 2>&1 ||
  ! security import "$certificate" -k "$keychain" -P "$APPLE_DEVELOPER_ID_P12_PASSWORD" \
    -T /usr/bin/codesign >/dev/null 2>&1 ||
  ! security set-key-partition-list -S apple-tool:,apple:,codesign: -s \
    -k "$keychain_password" "$keychain" >/dev/null 2>&1; then
  printf '%s\n' 'Could not prepare the temporary Apple signing keychain.' >&2
  exit 1
fi
# The child needs only the imported key and API key file, never their raw input.
unset APPLE_DEVELOPER_ID_P12_BASE64 APPLE_DEVELOPER_ID_P12_PASSWORD APPLE_NOTARY_KEY_P8
unset keychain_password
export SIGNING_MODE=developer-id APPLE_SIGNING_KEYCHAIN="$keychain"
export NOTARY_KEY_FILE="$notary_key" NOTARY_KEY_ID="$APPLE_NOTARY_KEY_ID"
export NOTARY_ISSUER_ID="$APPLE_NOTARY_ISSUER_ID"
# Give the child a private process group so cancellation also stops tools it
# launches. Teardown is explicit even when the child leaves background work.
set -m
"$@" &
child_pid=$!
set +m
status=0
wait "$child_pid" || status=$?
finish "$status"
