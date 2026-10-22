#!/bin/bash
# Sign and package an already built installer without running its payload.
# Credentials stay in the Keychain or an external API key file, never the app.

macos_signing_error() {
  printf 'macOS signing: %s\n' "$*" >&2
  return 1
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
  local path=$1 identifier=${2:-}
  local arguments=(--force --sign -)
  if [[ $SIGNING_MODE == developer-id ]]; then
    arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --options runtime)
    [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
  fi
  [[ -z $identifier ]] || arguments+=(--identifier "$identifier")
  codesign "${arguments[@]}" "$path" || return
  macos_verify_code "$path"
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

macos_staple() {
  xcrun stapler staple "$1" || return
  xcrun stapler validate "$1"
}

macos_assess_app() {
  syspolicy_check distribution "$1" --verbose || return
  spctl --assess --type execute --verbose=2 "$1"
}

macos_verify_installer_code() {
  local installer=$1 payload="$1/Contents/Resources/payload" name
  for name in agent-heavy agent-remote; do
    macos_verify_code "$payload/source/bin/$name" || return
  done
  macos_verify_code "$payload/Agent Queue.app" || return
  macos_verify_code "$installer" || return
  codesign --verify --strict --deep "$installer"
}

macos_verify_installer_distribution() {
  local installer=$1 app="$1/Contents/Resources/payload/Agent Queue.app"
  macos_verify_installer_code "$installer" || return
  xcrun stapler validate "$app" || return
  xcrun stapler validate "$installer" || return
  macos_assess_app "$app" || return
  macos_assess_app "$installer"
}

macos_verify_disk_image() {
  local image=$1 details
  codesign --verify --strict --test-requirement "$MACOS_SIGNING_REQUIREMENT" "$image" || return
  details=$(LC_ALL=C codesign --display --verbose=4 "$image" 2>&1) || return
  macos_verify_signer_details "$image" "$details" 0 || return
  xcrun stapler validate "$image" || return
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
    macos_sign_code "$payload/source/bin/$name" "io.went.agent-queue.$name" || return
  done
  macos_sign_code "$app" || return
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
  macos_sign_code "$installer" || return
  if [[ $SIGNING_MODE == developer-id ]]; then
    ditto -c -k --sequesterRsrc --keepParent "$installer" "$staging/installer.zip" || return
    macos_notarize installer "$staging/installer.zip" || return
    macos_staple "$installer" || return
    macos_assess_app "$installer" || return
  fi
  macos_verify_installer_code "$installer" || return
  ditto -c -k --sequesterRsrc --keepParent "$installer" "$staging/$base.zip" || return
  if [[ $build_dmg == 1 ]]; then
    sync || return
    hdiutil create -verbose -nospotlight -fs HFS+ -volname 'Agent Job Queue' \
      -srcfolder "$installer" -ov -format UDZO "$staging/$base.dmg" || return
    if [[ $SIGNING_MODE == developer-id ]]; then
      local image_signing_arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --identifier io.went.agent-queue.disk-image)
      [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || image_signing_arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
      codesign "${image_signing_arguments[@]}" "$staging/$base.dmg" || return
      codesign --verify --strict --test-requirement "$MACOS_SIGNING_REQUIREMENT" "$staging/$base.dmg" || return
      local details
      details=$(LC_ALL=C codesign --display --verbose=4 "$staging/$base.dmg" 2>&1) || return
      macos_verify_signer_details "$staging/$base.dmg" "$details" 0 || return
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
