#!/bin/bash
# Run one finite command with private, temporary Apple signing credentials.
set +x
set -euo pipefail
umask 077

# Reject untrusted hosted jobs before inspecting credentials or invoking tools.
if [[ ${GITHUB_ACTIONS:-} == true ]]; then
  case "${GITHUB_EVENT_NAME:-}" in
    push)
      [[ ${GITHUB_REF:-} == refs/heads/main ]] || {
        printf '%s\n' 'Apple signing requires a trusted push to main.' >&2
        exit 1
      }
      ;;
    workflow_run|workflow_dispatch)
      # These values come from the protected source-verification job, not from
      # downloaded build code. Keep the automation and source identities distinct.
      # shellcheck source=scripts/distribution-common.sh
      source "$(dirname -- "$0")/distribution-common.sh"
      distribution_context
      [[ ${GITHUB_REF_PROTECTED:-} == true &&
        ${GITHUB_WORKFLOW_REF:-} == "$GITHUB_REPOSITORY/.github/workflows/distribution.yml@refs/heads/main" &&
        ${AUTOMATION_SHA:-} == "$GITHUB_SHA" ]] || distribution_error 'Protected main distribution automation is required.'
      for variable in BUILD_RUN_ID BUILD_RUN_NUMBER BUILD_RUN_ATTEMPT BUILD_ARTIFACT_ID; do
        distribution_number "${!variable:-}" || distribution_error "Invalid $variable."
      done
      [[ ${BUILD_SHA:-} =~ ^[a-f0-9]{40}$ &&
        ${BUILD_ARTIFACT_DIGEST:-} =~ ^sha256:[a-f0-9]{64}$ &&
        ${BUILD_VERSION:-} =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]*$ &&
        ${BUILD_TAG:-} == "build-$BUILD_RUN_NUMBER-${BUILD_SHA:0:7}" ]] || distribution_error 'Verified original source artifact identity is required.'
      ;;
    *)
      printf '%s\n' 'Apple signing requires a trusted push to main or protected main distribution.' >&2
      exit 1
      ;;
  esac
  [[ ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || {
    printf '%s\n' 'Apple signing requires a GitHub-hosted runner.' >&2
    exit 1
  }
fi
if [[ $# == 0 ]]; then
  printf '%s\n' 'Usage: with-apple-signing.sh command [arguments...]' >&2
  exit 1
fi
if [[ -n ${APPLE_SIGNING_RESULT_FILE:-} ]]; then
  [[ $APPLE_SIGNING_RESULT_FILE == /* && ! -e $APPLE_SIGNING_RESULT_FILE && ! -L $APPLE_SIGNING_RESULT_FILE &&
    -d $(dirname "$APPLE_SIGNING_RESULT_FILE") && ${APPLE_SIGNING_OPERATION:-} =~ ^(payload|installer|dmg)-finish$ ]] || {
    printf '%s\n' 'A fresh external signing outcome path and fixed operation are required.' >&2
    exit 1
  }
fi
# shellcheck source=scripts/macos-keychain-search.sh
source "$(dirname -- "$0")/macos-keychain-search.sh"
for variable in APPLE_DEVELOPER_ID_P12_BASE64 APPLE_DEVELOPER_ID_P12_PASSWORD \
  APPLE_NOTARY_KEY_P8 APPLE_SIGNING_IDENTITY APPLE_TEAM_ID \
  APPLE_NOTARY_KEY_ID APPLE_NOTARY_ISSUER_ID; do
  if [[ -z ${!variable:-} ]]; then
    printf 'Apple signing requires %s.\n' "$variable" >&2
    exit 1
  fi
done

team_pattern='^[A-Z0-9]{10}$'
hash_pattern='^[A-Fa-f0-9]{40}$'
if [[ ! $APPLE_TEAM_ID =~ $team_pattern ]] ||
  [[ ! $APPLE_SIGNING_IDENTITY =~ $hash_pattern && $APPLE_SIGNING_IDENTITY != 'Developer ID Application: '* ]]; then
  printf '%s\n' 'Apple signing requires an exact Developer ID Application identity or SHA-1 and a ten-character team.' >&2
  exit 1
fi
identity_selector=$APPLE_SIGNING_IDENTITY
selector_hash=
if [[ $identity_selector =~ $hash_pattern ]]; then
  selector_hash=$(printf '%s' "$identity_selector" | LC_ALL=C tr '[:lower:]' '[:upper:]')
fi

# Parse only the requested inventory section. The unrestricted command also
# prints valid identities again; counting both sections would invent duplicates.
# Never echo tool output or certificate names, which need not be trusted text.
read_identity_inventory() {
  local file=$1 kind=$2 line hash name scanning=0 complete=0 count=0 summary
  local row_pattern='^[[:space:]]*[0-9]+\)[[:space:]]+([A-Fa-f0-9]{40})[[:space:]]+"([^"]+)"([[:space:]]+\([^[:cntrl:]]*\))?[[:space:]]*$'
  local summary_pattern='^[[:space:]]*([0-9]+) identities found[[:space:]]*$'
  inventory_matches=0 inventory_hash='' inventory_name=''
  if [[ $kind == valid ]]; then
    scanning=1
    summary_pattern='^[[:space:]]*([0-9]+) valid identities found[[:space:]]*$'
  fi
  while IFS= read -r line || [[ -n $line ]]; do
    [[ -n ${line//[[:space:]]/} ]] || continue
    if [[ $kind == full && $line == '  Matching identities' && $scanning == 0 ]]; then
      scanning=1
      continue
    fi
    if [[ $kind == full && $line == '  Valid identities only' ]]; then
      break
    fi
    if [[ $scanning == 0 ]]; then
      [[ $line == 'Policy: Code Signing' ]] || return 1
      continue
    fi
    [[ $complete == 0 ]] || return 1
    if [[ $line =~ $row_pattern ]]; then
      hash=${BASH_REMATCH[1]} name=${BASH_REMATCH[2]}
      [[ ! $name =~ [[:cntrl:]] ]] || return 1
      hash=$(printf '%s' "$hash" | LC_ALL=C tr '[:lower:]' '[:upper:]') || return 1
      count=$((count + 1))
      if [[ -n $selector_hash && $hash == "$selector_hash" ]] ||
        [[ -z $selector_hash && $name == "$identity_selector" ]]; then
        inventory_matches=$((inventory_matches + 1))
        inventory_hash=$hash inventory_name=$name
      fi
    elif [[ $line =~ $summary_pattern ]]; then
      summary=${BASH_REMATCH[1]}
      [[ $summary == "$count" ]] || return 1
      complete=1
    else
      return 1
    fi
  done < "$file"
  [[ $scanning == 1 && $complete == 1 ]]
}

identity_diagnostic() {
  printf 'Apple signing identity: phase=%s classification=%s matching=%s valid=%s' "$1" "$2" "$3" "$4" >&2
  if [[ $2 == ready ]]; then
    printf ' sha1=%s team=%s' "$5" "$APPLE_TEAM_ID" >&2
  fi
  printf '.\n' >&2
}

check_signing_identity() {
  local phase=$1 matching=unknown valid=unknown full_hash full_name valid_hash valid_name classification
  if ! LC_ALL=C security find-identity -p codesigning "$keychain" > "$work/identities" 2>/dev/null ||
    ! LC_ALL=C security find-identity -v -p codesigning "$keychain" > "$work/valid-identities" 2>/dev/null; then
    identity_diagnostic "$phase" query-failed "$matching" "$valid"
    return 1
  fi
  if ! read_identity_inventory "$work/identities" full; then
    identity_diagnostic "$phase" malformed-inventory "$matching" "$valid"
    return 1
  fi
  matching=$inventory_matches full_hash=$inventory_hash full_name=$inventory_name
  if ! read_identity_inventory "$work/valid-identities" valid; then
    identity_diagnostic "$phase" malformed-inventory "$matching" "$valid"
    return 1
  fi
  valid=$inventory_matches valid_hash=$inventory_hash valid_name=$inventory_name
  classification=ready
  if [[ $matching == 0 ]]; then classification=absent
  elif [[ $matching != 1 || $valid -gt 1 ]]; then classification=ambiguous
  elif [[ $full_name != 'Developer ID Application: '* ]]; then classification=wrong-type
  elif [[ $full_name != *" ($APPLE_TEAM_ID)" ]]; then classification=wrong-team
  elif [[ $valid == 0 ]]; then classification=invalid
  elif [[ $valid_hash != "$full_hash" || $valid_name != "$full_name" ]]; then classification=inconsistent
  fi
  identity_diagnostic "$phase" "$classification" "$matching" "$valid" "$full_hash"
  [[ $classification == ready ]] || return 1
  # Pin every subsequent codesign call to the unique validated imported identity.
  export APPLE_SIGNING_IDENTITY=$full_hash
}

work=
keychain=
keychain_created=0
child_pid=
cleanup() {
  local status=$1 command_status=$1 cleanup_status=success receipt
  trap - EXIT
  trap '' INT TERM
  if ! macos_keychain_search_restore; then
    status=1
    cleanup_status=failed
  fi
  if [[ -n $keychain ]] && [[ $keychain_created == 1 || -e $keychain ]]; then
    if ! security delete-keychain "$keychain" >/dev/null 2>&1; then
      printf '%s\n' 'Could not delete the temporary Apple signing keychain.' >&2
      status=1
      cleanup_status=failed
    fi
  fi
  if [[ -n $work ]] && ! rm -rf -- "$work"; then
    printf '%s\n' 'Could not remove temporary Apple signing credentials.' >&2
    status=1
    cleanup_status=failed
  fi
  if [[ -n ${APPLE_SIGNING_RESULT_FILE:-} ]]; then
    receipt=$(mktemp "$(dirname "$APPLE_SIGNING_RESULT_FILE")/.apple-outcome.XXXXXX") || exit 1
    if ! jq -n --arg cleanup "$cleanup_status" --arg operation "$APPLE_SIGNING_OPERATION" \
      --arg run "${GITHUB_RUN_ID:-}" --arg attempt "${GITHUB_RUN_ATTEMPT:-}" --argjson command_status "$command_status" \
      '{cleanup:$cleanup,operation:$operation,execution_id:$run,execution_attempt:$attempt,command_status:$command_status}' > "$receipt" ||
      ! ln "$receipt" "$APPLE_SIGNING_RESULT_FILE"; then
      rm -f "$receipt"
      exit 1
    fi
    rm -f "$receipt" || exit 1
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
if [[ ${GITHUB_ACTIONS:-} == true ]]; then
  macos_keychain_search_register "$keychain" || exit 1
fi
check_signing_identity preflight || exit 1
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
if [[ $status != 0 ]]; then
  check_signing_identity child-failure || true
fi
finish "$status"
