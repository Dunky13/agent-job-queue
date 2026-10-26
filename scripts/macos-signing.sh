#!/bin/bash
# Sign and package an already built installer without running its payload.
# Credentials stay in the Keychain or an external API key file, never the app.

macos_signing_error() {
  printf 'macOS signing: %s\n' "$*" >&2
  return 1
}

# Callers supply fixed operation labels, never paths or command arguments.
# Wrapped shell functions must explicitly return each failure: an if condition
# disables Bash errexit inside the called function as well as its descendants.
macos_signing_operation() {
  local operation=$1 status
  shift
  printf 'Apple signing operation: operation=%s phase=begin\n' "$operation" >&2
  if "$@"; then
    printf 'Apple signing operation: operation=%s phase=success status=0\n' "$operation" >&2
  else
    status=$?
    printf 'Apple signing operation: operation=%s phase=failure status=%s\n' "$operation" "$status" >&2
    return "$status"
  fi
}

macos_signing_configure() {
  SIGNING_MODE=${SIGNING_MODE:-adhoc}
  MACOS_NOTARY_AUTH=()
  case "$SIGNING_MODE" in
    adhoc)
      local name
      for name in APPLE_SIGNING_IDENTITY APPLE_SIGNING_KEYCHAIN APPLE_TEAM_ID NOTARY_KEYCHAIN_PROFILE NOTARY_KEYCHAIN \
        NOTARY_KEY_FILE NOTARY_KEY_ID NOTARY_ISSUER_ID NOTARY_TIMEOUT NOTARY_LOG_DIR; do
        if [[ -n ${!name:-} ]]; then
          macos_signing_error "$name requires SIGNING_MODE=developer-id; refusing an ad hoc fallback."
          return 1
        fi
      done
      return 0
      ;;
    developer-id) ;;
    *) macos_signing_error 'SIGNING_MODE must be adhoc or developer-id.'; return 1 ;;
  esac
  [[ -n ${APPLE_SIGNING_IDENTITY:-} && ${APPLE_SIGNING_IDENTITY:-} != - ]] || {
    macos_signing_error 'Developer ID signing requires APPLE_SIGNING_IDENTITY.'; return 1;
  }
  local team_pattern='^[A-Z0-9]{10}$' identity_pattern='^[A-Fa-f0-9]{40}$'
  [[ ${APPLE_TEAM_ID:-} =~ $team_pattern ]] || {
    macos_signing_error 'APPLE_TEAM_ID must be a ten-character Apple team identifier.'; return 1;
  }
  if [[ $APPLE_SIGNING_IDENTITY != 'Developer ID Application: '* && ! $APPLE_SIGNING_IDENTITY =~ $identity_pattern ]]; then
    macos_signing_error 'Use a Developer ID Application identity or its certificate SHA-1.'; return 1
  fi
  if [[ -n ${APPLE_SIGNING_KEYCHAIN:-} && ( ! -f $APPLE_SIGNING_KEYCHAIN || ! -r $APPLE_SIGNING_KEYCHAIN ) ]]; then
    macos_signing_error 'APPLE_SIGNING_KEYCHAIN must be a readable signing Keychain file.'; return 1
  fi
  if [[ -n ${NOTARY_KEYCHAIN_PROFILE:-} ]]; then
    [[ -z ${NOTARY_KEY_FILE:-}${NOTARY_KEY_ID:-}${NOTARY_ISSUER_ID:-} ]] || {
      macos_signing_error 'Keychain and API key notarization credentials cannot be mixed.'; return 1;
    }
    MACOS_NOTARY_AUTH=(--keychain-profile "$NOTARY_KEYCHAIN_PROFILE")
    if [[ -n ${NOTARY_KEYCHAIN:-} ]]; then
      [[ -f $NOTARY_KEYCHAIN && -r $NOTARY_KEYCHAIN ]] || {
        macos_signing_error 'NOTARY_KEYCHAIN must be a readable Keychain file.'; return 1;
      }
      MACOS_NOTARY_AUTH+=(--keychain "$NOTARY_KEYCHAIN")
    fi
  else
    [[ -z ${NOTARY_KEYCHAIN:-} && -n ${NOTARY_KEY_FILE:-} && -n ${NOTARY_KEY_ID:-} ]] || {
      macos_signing_error 'Provide a Keychain profile or an API key file and key ID.'; return 1;
    }
    [[ -f $NOTARY_KEY_FILE && -r $NOTARY_KEY_FILE ]] || {
      macos_signing_error 'NOTARY_KEY_FILE must be a readable external API key file.'; return 1;
    }
    local key_pattern='^[A-Za-z0-9]+$' issuer_pattern='^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$'
    [[ $NOTARY_KEY_ID =~ $key_pattern ]] || {
      macos_signing_error 'NOTARY_KEY_ID must be an alphanumeric Apple API key identifier.'; return 1;
    }
    MACOS_NOTARY_AUTH=(--key "$NOTARY_KEY_FILE" --key-id "$NOTARY_KEY_ID")
    if [[ -n ${NOTARY_ISSUER_ID:-} ]]; then
      [[ $NOTARY_ISSUER_ID =~ $issuer_pattern ]] || {
        macos_signing_error 'NOTARY_ISSUER_ID must be an Apple issuer UUID.'; return 1;
      }
      MACOS_NOTARY_AUTH+=(--issuer "$NOTARY_ISSUER_ID")
    fi
  fi
  NOTARY_TIMEOUT=${NOTARY_TIMEOUT:-30m}
  local timeout_pattern='^[1-9][0-9]*[smh]?$'
  [[ $NOTARY_TIMEOUT =~ $timeout_pattern ]] || {
    macos_signing_error 'NOTARY_TIMEOUT must be a positive duration such as 30m.'; return 1;
  }
  macos_signing_verification_configure "$APPLE_TEAM_ID"
}

macos_signing_verification_configure() {
  local team_pattern='^[A-Z0-9]{10}$'
  [[ $1 =~ $team_pattern ]] || {
    macos_signing_error 'Expected team must be a ten-character Apple team identifier.'; return 1;
  }
  SIGNING_MODE=developer-id
  APPLE_TEAM_ID=$1
  # This requires an Apple Developer ID Application certificate, not just a
  # self-signed certificate that happens to claim the expected team identifier.
  MACOS_SIGNING_REQUIREMENT="=anchor apple generic and certificate leaf[subject.OU] = \"$APPLE_TEAM_ID\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists"
}

macos_signing_canonical_path() {
  # realpath requires existing parents on macOS. Resolve each existing component
  # and normalize missing suffixes too, before creating output/log directories.
  local path=$1 resolved=/ remaining component next
  [[ $path == /* ]] || path="$PWD/$path"
  if [[ -e $path || -L $path ]]; then
    realpath -q "$path"
    return
  fi
  remaining=${path#/}
  while [[ -n $remaining ]]; do
    component=${remaining%%/*}
    if [[ $remaining == */* ]]; then remaining=${remaining#*/}; else remaining=; fi
    case "$component" in
      ''|.) continue ;;
      ..) resolved=${resolved%/*}; resolved=${resolved:-/} ;;
      *)
        next="${resolved%/}/$component"
        if [[ -e $next || -L $next ]]; then
          resolved=$(realpath -q "$next") || return
        else
          resolved=$next
        fi
        ;;
    esac
  done
  printf '%s\n' "$resolved"
}

macos_signing_validate_paths() {
  local installer destination logs name private_path
  installer=$(macos_signing_canonical_path "$1") || return
  destination=$(macos_signing_canonical_path "$2") || return
  logs=$(macos_signing_canonical_path "${NOTARY_LOG_DIR:-"${2%/}.notarization"}") || return
  for name in APPLE_SIGNING_KEYCHAIN NOTARY_KEYCHAIN NOTARY_KEY_FILE; do
    [[ -n ${!name:-} ]] || continue
    private_path=$(macos_signing_canonical_path "${!name}") || return
    case "$private_path/" in
      "$installer/"*|"$destination/"*)
        macos_signing_error "$name must be outside the prepared installer and artifact destination."; return 1 ;;
    esac
  done
  case "$destination/" in
    "$installer/"*) macos_signing_error 'The artifact destination must be outside the prepared installer.'; return 1 ;;
  esac
  case "$logs/" in
    "$installer/"*|"$destination/"*)
      macos_signing_error 'NOTARY_LOG_DIR must be outside the prepared installer and artifact destination.'; return 1 ;;
  esac
  MACOS_CANONICAL_LOG_ROOT=$logs
}

macos_signing_prepare_logs() {
  local destination=$1 log_root=$MACOS_CANONICAL_LOG_ROOT
  mkdir -p "$destination" || return
  destination=$(cd -- "$destination" && pwd -P) || return
  (umask 077; mkdir -p "$log_root") || return
  log_root=$(cd -- "$log_root" && pwd -P) || return
  case "$log_root/" in
    "$destination/"*) macos_signing_error 'NOTARY_LOG_DIR must be outside the artifact destination.'; return 1 ;;
  esac
  MACOS_NOTARY_LOG_DIR=$(umask 077; mktemp -d "$log_root/notarization.XXXXXX") || return
  printf 'Notarization diagnostics retained in %s\n' "$MACOS_NOTARY_LOG_DIR" >&2
}

macos_verify_signer_details() {
  local path=$1 details=$2 runtime=$3
  if ! printf '%s\n' "$details" | grep -Fxq "TeamIdentifier=$APPLE_TEAM_ID" ||
    ! printf '%s\n' "$details" | grep -q '^Authority=Developer ID Application:' ||
    ! printf '%s\n' "$details" | grep -q '^Timestamp=.'; then
    macos_signing_error "Expected Developer ID team and secure timestamp are missing: $path"; return 1
  fi
  if [[ $runtime == 1 ]] && ! printf '%s\n' "$details" | grep -Eq '^CodeDirectory .*flags=.*\([^)]*runtime[^)]*\)'; then
    macos_signing_error "Hardened Runtime is missing: $path"; return 1
  fi
}

macos_verify_code() {
  local path=$1 architecture details
  if [[ $SIGNING_MODE == adhoc ]]; then
    codesign --verify --strict --all-architectures "$path" || return
    return 0
  fi
  codesign --verify --strict --all-architectures --test-requirement "$MACOS_SIGNING_REQUIREMENT" "$path" || return
  # Display defaults to the host architecture. Inspect both slices explicitly.
  for architecture in arm64 x86_64; do
    details=$(LC_ALL=C codesign --display --verbose=4 --architecture "$architecture" "$path" 2>&1) || return
    macos_verify_signer_details "$path ($architecture)" "$details" 1 || return
  done
}

macos_sign_code() {
  local path=$1 identifier=${2:-} operation=${3:-code}
  case "$operation" in
    code|agent-heavy|agent-remote|app-agent-heavy|app-agent-remote|app|installer) ;;
    *) macos_signing_error 'Unknown signing operation.'; return 1 ;;
  esac
  local arguments=(--force --sign -)
  if [[ $SIGNING_MODE == developer-id ]]; then
    arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --options runtime)
    [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
  fi
  [[ -z $identifier ]] || arguments+=(--identifier "$identifier")
  macos_signing_operation "sign-$operation" codesign "${arguments[@]}" "$path" || return
  macos_signing_operation "verify-$operation" macos_verify_code "$path"
}

macos_notarize() {
  local stage=$1 archive=$2 prefix="$MACOS_NOTARY_LOG_DIR/$1" submission status
  local submit_result=0 wait_result=0 log_result=0
  printf 'Submitting %s for Apple notarization.\n' "$stage" >&2
  # Submit separately so an interruption or a long first submission cannot
  # discard the identifier needed to inspect or resume Apple's processing.
  (umask 077; xcrun notarytool submit "$archive" "${MACOS_NOTARY_AUTH[@]}" \
    --output-format json --no-progress > "$prefix.submission.json" 2> "$prefix.submit.stderr") || submit_result=$?
  if [[ $submit_result != 0 ]]; then
    macos_signing_error "$stage upload failed; inspect $prefix.submit.stderr and $prefix.submission.json"; return 1
  fi
  submission=$(plutil -extract id raw -expect string -o - "$prefix.submission.json" 2>/dev/null) || {
    macos_signing_error "$stage upload did not return a submission identifier; inspect $prefix.submission.json"; return 1;
  }
  local id_pattern='^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$'
  [[ $submission =~ $id_pattern ]] || {
    macos_signing_error "$stage returned an invalid submission identifier; inspect $prefix.submission.json"; return 1;
  }
  printf 'Apple submission %s (%s); waiting up to %s.\n' "$submission" "$stage" "$NOTARY_TIMEOUT" >&2
  (umask 077; xcrun notarytool wait "$submission" "${MACOS_NOTARY_AUTH[@]}" \
    --timeout "$NOTARY_TIMEOUT" --output-format json > "$prefix.status.json" 2> "$prefix.wait.stderr") || wait_result=$?
  # Successful submissions may still contain warnings. Always retain the log.
  (umask 077; xcrun notarytool log "$submission" "${MACOS_NOTARY_AUTH[@]}" "$prefix.log.json" \
    > "$prefix.log.stdout" 2> "$prefix.log.stderr") || log_result=$?
  if [[ $wait_result != 0 ]]; then
    macos_signing_error "$stage processing did not complete successfully; submission $submission remains recorded in $prefix.submission.json. Inspect $prefix.wait.stderr and $prefix.log.json"; return 1
  fi
  status=$(plutil -extract status raw -expect string -o - "$prefix.status.json" 2>/dev/null) || {
    macos_signing_error "$stage did not return a notarization status; inspect $prefix.status.json"; return 1;
  }
  [[ $status == Accepted ]] || {
    macos_signing_error "$stage was not Accepted; inspect $prefix.status.json and $prefix.log.json"; return 1;
  }
  [[ $log_result == 0 ]] || {
    macos_signing_error "$stage was Accepted but its diagnostic log could not be retrieved; inspect $prefix.log.stderr"; return 1;
  }
}

# Stapling and validation both fetch Apple's current ticket. Retry only the
# observed CloudKit connection loss, never missing, invalid or revoked tickets.
macos_ticket_connection_lost() {
  # Read only the outer UserInfo fields. Nested errors and quoted descriptions
  # cannot supply the failing URL for the qualifying outer NSError. Retain
  # their delimiters so excluded content cannot join surrounding URL tokens.
  awk '
    function lookup(line, value, keys) {
      keys = 0
      while (match(line, /(^|,)[[:space:]]*NSErrorFailingURLStringKey=/)) {
        line = substr(line, RSTART + RLENGTH)
        value = line
        if (match(value, /,[[:space:]]*[[:alpha:]_][[:alnum:]_]*=/)) value = substr(value, 1, RSTART - 1)
        sub(/^[[:space:]]+/, "", value)
        sub(/[[:space:]]+$/, "", value)
        if (value != "https://api.apple-cloudkit.com/database/1/com.apple.gk.ticket-delivery/production/public/records/lookup") return 0
        keys++
      }
      return keys == 1
    }
    function scan(line, i, c) {
      for (i = 1; i <= length(line); i++) {
        c = substr(line, i, 1)
        if (quoted) {
          if (escaped) escaped = 0
          else if (c == "\\") escaped = 1
          else if (c == "\"") {
            quoted = 0
            if (depth == 1) own = own c
          }
          continue
        } else if (c == "\"") {
          quoted = 1
          if (depth == 1) own = own c
          continue
        }
        else if (depth == 0) {
          if ((i == 1 || substr(line, i - 1, 1) ~ /[[:space:]]/) &&
              substr(line, i, length("UserInfo={")) == "UserInfo={") {
            depth = 1
            i += length("UserInfo={") - 1
          }
          continue
        }
        else if (c == "{") {
          if (depth == 1) own = own c
          depth++
        }
        else if (c == "}") {
          depth--
          if (depth == 0) {
            if (lookup(own)) found = 1
            active = 0
            return
          }
        }
        if (depth == 1) own = own c
      }
      own = own " "
    }
    /^error is Error Domain=/ {
      active = ($0 ~ /^error is Error Domain=NSURLErrorDomain Code=-1005 /)
      depth = 0; quoted = 0; escaped = 0; own = ""
      if (active) scan($0)
      next
    }
    active && (/^[[:space:]]/ || /^\),/) { scan($0); next }
    { active = 0 }
    END { exit !found }
  ' "$1"
}

macos_ticket_operation() (
  local operation=$1 target=$2 output status attempt=1 delay=5 max_attempts=6
  output=$(mktemp) || return
  trap 'rm -f "$output"' EXIT
  while :; do
    if xcrun stapler "$operation" "$target" > "$output" 2>&1; then
      cat "$output" || return
      return 0
    else
      status=$?
    fi
    cat "$output" || return
    if [[ $status != 68 ]] || ! macos_ticket_connection_lost "$output"; then
      return "$status"
    fi
    if [[ $attempt -ge $max_attempts ]]; then
      printf 'Apple ticket %s: CloudKit connection lost; exhausted %s attempts.\n' "$operation" "$max_attempts" >&2
      return "$status"
    fi
    printf 'Apple ticket %s: CloudKit connection lost; retrying after %s seconds (attempt %s of %s).\n' "$operation" "$delay" "$((attempt + 1))" "$max_attempts" >&2
    sleep "$delay" || return
    attempt=$((attempt + 1))
    delay=$((delay * 2))
  done
)

macos_validate_ticket() {
  macos_ticket_operation validate "$1"
}

macos_staple() {
  macos_ticket_operation staple "$1" || return
  macos_validate_ticket "$1"
}

macos_assess_app() {
  syspolicy_check distribution "$1" --verbose || return
  spctl --assess --type execute --verbose=2 "$1"
}

macos_verify_installer_code() {
  local installer=$1 payload="$1/Contents/Resources/payload" name embedded
  embedded="$payload/Agent Queue.app/Contents/Resources/payload/source"
  macos_app_payload_mode "$payload/Agent Queue.app" || return
  for name in agent-heavy agent-remote; do
    macos_verify_code "$payload/source/bin/$name" || return
    if [[ $MACOS_APP_EMBEDDED == 1 ]]; then
      macos_verify_code "$embedded/bin/$name" || return
    fi
  done
  macos_verify_code "$payload/Agent Queue.app" || return
  macos_verify_code "$installer" || return
  codesign --verify --strict --deep "$installer"
}

# Old retained build artifacts lack self-contained menu apps. Keep their custom
# installer packaging intact when newer distribution automation replays them.
macos_app_payload_mode() {
  local app=$1 payload="$1/Contents/Resources/payload" name
  MACOS_APP_EMBEDDED=0
  if [[ -L $payload ]]; then
    macos_signing_error 'The menu app setup payload must not be a symlink.'; return 1;
  fi
  if [[ ! -e $payload ]]; then
    if plutil -extract AgentQueueEmbeddedPayload raw -o - "$app/Contents/Info.plist" >/dev/null 2>&1; then
      macos_signing_error 'The menu app declares an embedded setup payload but it is missing.'; return 1;
    fi
    return 0
  fi
  for name in "$payload" "$payload/source" "$payload/source/bin"; do
    [[ -d $name && ! -L $name ]] || {
      macos_signing_error 'The menu app setup payload is incomplete or linked.'; return 1;
    }
  done
  [[ -f "$payload/source/build-info.json" && ! -L "$payload/source/build-info.json" ]] || {
    macos_signing_error 'The menu app setup payload is incomplete.'; return 1;
  }
  for name in agent-heavy agent-remote; do
    [[ -f "$payload/source/bin/$name" && ! -L "$payload/source/bin/$name" && -x "$payload/source/bin/$name" ]] || {
      macos_signing_error 'The menu app setup helper is missing or not executable.'; return 1;
    }
  done
  MACOS_APP_EMBEDDED=1
}

macos_sign_app_helpers() {
  local app=$1 source="$1/Contents/Resources/payload/source" name
  macos_app_payload_mode "$app" || return
  [[ $MACOS_APP_EMBEDDED == 1 ]] || return 0
  for name in agent-heavy agent-remote; do
    macos_sign_code "$source/bin/$name" "io.went.agent-queue.$name" "app-$name" || return
  done
}

macos_disk_image_source() {
  local installer=$1 directory=$2 app="$1/Contents/Resources/payload/Agent Queue.app"
  MACOS_DISK_IMAGE_SOURCE=$installer
  macos_app_payload_mode "$app" || return
  [[ $MACOS_APP_EMBEDDED == 1 ]] || return 0
  mkdir "$directory" || return
  ditto --rsrc --extattr --qtn --acl "$app" "$directory/Agent Queue.app" || return
  ln -s /Applications "$directory/Applications" || return
  MACOS_DISK_IMAGE_SOURCE=$directory
}

macos_verify_installer_distribution() {
  local installer=$1 app="$1/Contents/Resources/payload/Agent Queue.app"
  macos_verify_installer_code "$installer" || return
  macos_validate_ticket "$app" || return
  macos_validate_ticket "$installer" || return
  macos_assess_app "$app" || return
  macos_assess_app "$installer"
}

macos_verify_disk_image_code() {
  local image=$1 details
  codesign --verify --strict --test-requirement "$MACOS_SIGNING_REQUIREMENT" "$image" || return
  details=$(LC_ALL=C codesign --display --verbose=4 "$image" 2>&1) || return
  macos_verify_signer_details "$image" "$details" 0
}

macos_verify_disk_image() {
  local image=$1
  macos_verify_disk_image_code "$image" || return
  macos_validate_ticket "$image" || return
  spctl --assess --type open --context context:primary-signature --verbose=2 "$image" || return
  hdiutil verify "$image"
}

macos_package_installer() (
  # The packaging worker owns only this temporary directory. Notary diagnostics
  # live elsewhere and survive failures, including Apple's first-time delays.
  local installer=$1 destination=$2 version=$3 build_dmg=${BUILD_DMG:-1}
  local payload="$1/Contents/Resources/payload" app="$1/Contents/Resources/payload/Agent Queue.app"
  local version_pattern='^[A-Za-z0-9][A-Za-z0-9.+_-]*$' staging base name
  [[ $version =~ $version_pattern && ( $build_dmg == 0 || $build_dmg == 1 ) ]] || {
    macos_signing_error 'Provide a filename-safe version and BUILD_DMG=0 or 1.'; return 1;
  }
  [[ -d $installer && -d $app && -f $payload/source/bin/agent-heavy && -f $payload/source/bin/agent-remote ]] || {
    macos_signing_error 'The prepared installer and its complete payload are required.'; return 1;
  }
  if [[ $SIGNING_MODE == developer-id ]]; then
    macos_signing_validate_paths "$installer" "$destination" || return
  fi
  staging=$(mktemp -d) || return
  trap 'rm -rf "$staging"' EXIT
  base="Agent-Job-Queue-Installer-$version-universal"
  if [[ $SIGNING_MODE == developer-id ]]; then
    macos_signing_prepare_logs "$destination" || return
  fi
  for name in agent-heavy agent-remote; do
    macos_sign_code "$payload/source/bin/$name" "io.went.agent-queue.$name" "$name" || return
  done
  macos_sign_app_helpers "$app" || return
  macos_sign_code "$app" '' app || return
  if [[ $SIGNING_MODE == developer-id ]]; then
    # Apple's custom installer workflow requires the installed product to be
    # notarized first. Bare helper binaries receive tickets but cannot be stapled.
    ditto -c -k --sequesterRsrc --keepParent "$payload" "$staging/payload.zip" || return
    macos_notarize payload "$staging/payload.zip" || return
    macos_staple "$app" || return
    macos_verify_code "$app" || return
    macos_assess_app "$app" || return
  fi
  # The payload is sealed as installer resources. Do not staple or otherwise
  # modify its contents after signing the parent installer.
  macos_sign_code "$installer" '' installer || return
  if [[ $SIGNING_MODE == developer-id ]]; then
    ditto -c -k --sequesterRsrc --keepParent "$installer" "$staging/installer.zip" || return
    macos_notarize installer "$staging/installer.zip" || return
    macos_staple "$installer" || return
    macos_assess_app "$installer" || return
  fi
  macos_verify_installer_code "$installer" || return
  ditto -c -k --sequesterRsrc --keepParent "$installer" "$staging/$base.zip" || return
  if [[ $build_dmg == 1 ]]; then
    macos_disk_image_source "$installer" "$staging/image" || return
    sync || return
    hdiutil create -verbose -nospotlight -fs HFS+ -volname 'Agent Job Queue' \
      -srcfolder "$MACOS_DISK_IMAGE_SOURCE" -ov -format UDZO "$staging/$base.dmg" || return
    if [[ $SIGNING_MODE == developer-id ]]; then
      local image_signing_arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --identifier io.went.agent-queue.disk-image)
      [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || image_signing_arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
      macos_signing_operation sign-dmg codesign "${image_signing_arguments[@]}" "$staging/$base.dmg" || return
      macos_signing_operation verify-dmg macos_verify_disk_image_code "$staging/$base.dmg" || return
      macos_notarize dmg "$staging/$base.dmg" || return
      macos_staple "$staging/$base.dmg" || return
      macos_verify_disk_image "$staging/$base.dmg" || return
    else
      hdiutil verify "$staging/$base.dmg" || return
    fi
  fi
  # No intermediate or unnotarized artifacts are published. Hash the final
  # stapled bytes, and only then copy the completed assets into the destination.
  (cd "$staging" && shasum -a 256 "$base.zip" > "$base.zip.sha256") || return
  if [[ $build_dmg == 1 ]]; then
    (cd "$staging" && shasum -a 256 "$base.dmg" > "$base.dmg.sha256") || return
  fi
  mkdir -p "$destination" || return
  mv "$staging/$base.zip" "$staging/$base.zip.sha256" "$destination/" || return
  if [[ $build_dmg == 1 ]]; then
    mv "$staging/$base.dmg" "$staging/$base.dmg.sha256" "$destination/" || return
  fi
  printf 'Built %s\n' "$destination/$base.zip"
)

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -euo pipefail
  case "${1:-}" in
    package)
      [[ $# == 4 ]] || { printf 'Usage: macos-signing.sh package INSTALLER_APP DESTINATION VERSION\n' >&2; exit 2; }
      macos_signing_configure
      macos_package_installer "$2" "$3" "$4"
      ;;
    verify-installer)
      [[ $# == 3 ]] || { printf 'Usage: macos-signing.sh verify-installer INSTALLER_APP EXPECTED_TEAM\n' >&2; exit 2; }
      macos_signing_verification_configure "$3"
      macos_verify_installer_distribution "$2"
      ;;
    verify-dmg)
      [[ $# == 3 ]] || { printf 'Usage: macos-signing.sh verify-dmg DISK_IMAGE EXPECTED_TEAM\n' >&2; exit 2; }
      macos_signing_verification_configure "$3"
      macos_verify_disk_image "$2"
      ;;
    *) printf 'Usage: macos-signing.sh {package|verify-installer|verify-dmg} ...\n' >&2; exit 2 ;;
  esac
fi
