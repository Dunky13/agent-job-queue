#!/bin/bash
# Resume a single release run without rebuilding or resubmitting signed bytes.
set -euo pipefail
umask 077
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/macos-signing.sh
source "$root/scripts/macos-signing.sh"

fail() { macos_signing_error "$*"; exit 1; }
sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
json_field() { jq -er ".$2" "$1"; }
atomic_json() { jq "$2" "$1" > "$1.new"; mv "$1.new" "$1"; }
archive() { ditto -c -k --sequesterRsrc --keepParent "$1" "$2"; }

identity() {
  local sha_pattern='^[a-f0-9]{40}$' number_pattern='^[1-9][0-9]*$'
  local repository_pattern='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' version_pattern='^[A-Za-z0-9][A-Za-z0-9.+_-]*$'
  [[ ${GITHUB_SHA:-} =~ $sha_pattern && ${GITHUB_REPOSITORY:-} =~ $repository_pattern &&
    ${GITHUB_RUN_ID:-} =~ $number_pattern && ${GITHUB_RUN_NUMBER:-} =~ $number_pattern &&
    ${GITHUB_RUN_ATTEMPT:-} =~ $number_pattern && $resume_version =~ $version_pattern ]] ||
    fail 'A valid version and exact GitHub run identity are required.'
  macos_signing_verification_configure "${APPLE_TEAM_ID:-}"
}

lock_state() {
  [[ -d $state && ! -L $state ]] || fail 'State directory is missing or is a symlink.'
  mkdir "$state/.lock" 2>/dev/null || fail 'State is locked; do not use a snapshot of an active command.'
  printf '%s\n' "$$" > "$state/.lock/owner"
  trap 'if [[ -f $state/.lock/owner && $(cat "$state/.lock/owner") == "$$" ]]; then rm -rf "$state/work" "$state/.lock"; fi' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

check_file() {
  [[ -f $1 && ! -L $1 ]] || fail 'An expected immutable state file is missing or is a symlink.'
  [[ $(sha256 "$1") == "$2" ]] || fail 'Immutable artifact checksum mismatch.'
}

check_receipt() {
  local stage=$1 record="$state/stages/$1.json" receipt="$state/receipts/$1.json"
  [[ -f $receipt && ! -L $receipt ]] || fail "$stage acceptance receipt is missing."
  jq -e --arg id "$(json_field "$record" submission_id)" --arg hash "$(json_field "$record" archive_sha256)" '
    keys == ["jobId", "sha256", "status"] and .status == "Accepted" and .jobId == $id and .sha256 == $hash
  ' "$receipt" >/dev/null || fail "$stage acceptance receipt does not match its submission and exact archive."
  local log="$state/receipts/$stage.log.json"
  [[ -f $log && ! -L $log ]] || fail "$stage accepted Apple log is missing."
  jq -e --arg id "$(json_field "$record" submission_id)" --arg hash "$(json_field "$record" archive_sha256)" '
    .status == "Accepted" and .jobId == $id and .sha256 == $hash
  ' "$log" >/dev/null || fail "$stage accepted Apple log does not match its exact submission and archive."
}

verify_state() {
  local metadata="$state/metadata.json" stage phase previous=finished record hash
  [[ -f $metadata && ! -L $metadata ]] || fail 'State metadata is missing.'
  jq -e --arg version "$resume_version" --arg team "$APPLE_TEAM_ID" --arg sha "$GITHUB_SHA" \
    --arg repository "$GITHUB_REPOSITORY" --arg run "$GITHUB_RUN_ID" --arg number "$GITHUB_RUN_NUMBER" '
    keys == ["initial_sha256", "repository", "run_id", "run_number", "schema", "source_sha", "team", "version"] and
    .schema == 1 and .version == $version and .team == $team and .source_sha == $sha and
    .repository == $repository and .run_id == $run and .run_number == $number and
    (.initial_sha256 | type == "string" and test("^[a-f0-9]{64}$"))
  ' "$metadata" >/dev/null || fail 'State metadata does not match this release, team, repository, commit and run.'
  local directory
  for directory in archives completed stages receipts; do
    [[ -d $state/$directory && ! -L $state/$directory ]] || fail 'An expected state directory is missing or is a symlink.'
  done
  check_file "$state/initial.zip" "$(json_field "$metadata" initial_sha256)"
  for stage in payload installer dmg; do
    record="$state/stages/$stage.json"
    if [[ ! -e $record ]]; then
      [[ ! -e $state/archives/$stage.zip && ! -e $state/archives/$stage.dmg &&
        ! -e $state/completed/$stage.zip && ! -e $state/completed/$stage.dmg &&
        ! -e $state/receipts/$stage.json ]] || fail "$stage has artifacts without a state record."
      previous=missing
      continue
    fi
    [[ $previous == finished && -f $record && ! -L $record ]] || fail 'State stages are out of order.'
    jq -e --arg stage "$stage" '
      keys == ["archive_sha256", "completed_sha256", "phase", "prepared_attempt", "stage", "submission_id"] and
      .stage == $stage and (.prepared_attempt | type == "string" and test("^[1-9][0-9]*$")) and
      (.archive_sha256 | type == "string" and test("^[a-f0-9]{64}$")) and
      (.phase == "prepared" or .phase == "uploading" or .phase == "submitted" or .phase == "accepted" or .phase == "finished") and
      (if .phase == "prepared" or .phase == "uploading" then .submission_id == "" else
        (.submission_id | type == "string" and test("^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$")) end) and
      (if .phase == "finished" then (.completed_sha256 | type == "string" and test("^[a-f0-9]{64}$")) else .completed_sha256 == "" end)
    ' "$record" >/dev/null || fail "$stage state record is malformed or its submission ID is missing."
    phase=$(json_field "$record" phase)
    hash=$(json_field "$record" archive_sha256)
    if [[ $stage == dmg ]]; then check_file "$state/archives/dmg.dmg" "$hash";
    else check_file "$state/archives/$stage.zip" "$hash"; fi
    case "$phase" in accepted|finished) check_receipt "$stage" ;; esac
    if [[ $phase == finished ]]; then
      hash=$(json_field "$record" completed_sha256)
      if [[ $stage == dmg ]]; then check_file "$state/completed/dmg.dmg" "$hash";
      else check_file "$state/completed/$stage.zip" "$hash"; fi
    fi
    previous=$phase
  done
}

configure_credentials() {
  SIGNING_MODE=developer-id
  NOTARY_TIMEOUT=${NOTARY_TIMEOUT:-5m}
  macos_signing_configure
  local name path
  for name in APPLE_SIGNING_KEYCHAIN NOTARY_KEYCHAIN NOTARY_KEY_FILE; do
    [[ -n ${!name:-} ]] || continue
    path=$(macos_signing_canonical_path "${!name}")
    case "$path/" in "$state/"*) fail 'Signing credentials must remain outside resumable state.' ;; esac
  done
}

require_stage() {
  case "$stage" in payload|installer|dmg) ;; *) fail 'Stage must be payload, installer or dmg.' ;; esac
  record="$state/stages/$stage.json"
  if [[ $stage == dmg ]]; then submitted_archive="$state/archives/dmg.dmg"; else submitted_archive="$state/archives/$stage.zip"; fi
}

restore_installer() {
  rm -rf "$state/work"
  mkdir "$state/work"
  ditto -x -k "$state/initial.zip" "$state/work"
  installer="$state/work/Agent Job Queue Installer.app"
  payload="$installer/Contents/Resources/payload"
  app="$payload/Agent Queue.app"
}

prepare_stage() {
  local previous name details image_signing_arguments
  [[ ! -f $record ]] || { printf '%s already prepared.\n' "$stage"; return; }
  case "$stage" in payload) ;; installer) previous=payload ;; dmg) previous=installer ;; esac
  if [[ $stage != payload ]]; then
    [[ -f $state/stages/$previous.json && $(json_field "$state/stages/$previous.json" phase) == finished ]] ||
      fail 'The previous stage must be accepted and finished before preparing this stage.'
  fi
  configure_credentials
  restore_installer
  case "$stage" in
    payload)
      for name in agent-heavy agent-remote; do
        macos_sign_code "$payload/source/bin/$name" "io.went.agent-queue.$name"
      done
      macos_sign_code "$app"
      archive "$payload" "$submitted_archive"
      ;;
    installer)
      rm -rf "$payload"
      ditto -x -k "$state/completed/payload.zip" "$installer/Contents/Resources"
      macos_sign_code "$installer"
      archive "$installer" "$submitted_archive"
      ;;
    dmg)
      rm -rf "$state/work/Agent Job Queue Installer.app"
      ditto -x -k "$state/completed/installer.zip" "$state/work"
      macos_verify_installer_distribution "$installer"
      sync
      hdiutil create -verbose -nospotlight -fs HFS+ -volname 'Agent Job Queue' \
        -srcfolder "$installer" -ov -format UDZO "$submitted_archive"
      image_signing_arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --identifier io.went.agent-queue.disk-image)
      [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || image_signing_arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
      codesign "${image_signing_arguments[@]}" "$submitted_archive"
      codesign --verify --strict --test-requirement "$MACOS_SIGNING_REQUIREMENT" "$submitted_archive"
      details=$(LC_ALL=C codesign --display --verbose=4 "$submitted_archive" 2>&1)
      macos_verify_signer_details "$submitted_archive" "$details" 0
      ;;
  esac
  jq -n --arg stage "$stage" --arg attempt "$GITHUB_RUN_ATTEMPT" --arg hash "$(sha256 "$submitted_archive")" '
    {stage:$stage, phase:"prepared", prepared_attempt:$attempt, archive_sha256:$hash, submission_id:"", completed_sha256:""}
  ' > "$record.new"
  mv "$record.new" "$record"
  rm -rf "$state/work"
}

submit_stage() {
  [[ -f $record ]] || fail 'Prepare this stage before submitting it.'
  [[ $(json_field "$record" phase) != uploading ]] || fail 'A previous upload has an unknown outcome; manual reconciliation is required, never resubmit.'
  [[ $(json_field "$record" phase) == prepared ]] || { printf '%s submission already recorded.\n' "$stage"; return; }
  [[ $(json_field "$record" prepared_attempt) == "$GITHUB_RUN_ATTEMPT" ]] ||
    fail 'Prepared snapshot has no submission ID from an earlier attempt. Upload outcome is unknown; reconcile with Apple manually, never resubmit.'
  configure_credentials
  local response submission id_pattern='^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$'
  response=$(mktemp)
  # Raw provider output stays outside the durable public state. Do not retain
  # stderr, auth arguments or private runner paths in snapshots.
  atomic_json "$record" '.phase="uploading"'
  if ! xcrun notarytool submit "$submitted_archive" "${MACOS_NOTARY_AUTH[@]}" --output-format json --no-progress > "$response"; then
    rm -f "$response"
    fail 'Notary upload failed or its outcome is unknown; preserve the prepared snapshot for manual reconciliation.'
  fi
  submission=$(jq -er '.id | strings' "$response") || { rm -f "$response"; fail 'Upload returned no submission ID; manual reconciliation is required.'; }
  rm -f "$response"
  [[ $submission =~ $id_pattern ]] || fail 'Upload returned a malformed submission ID; manual reconciliation is required.'
  jq --arg id "$submission" '.phase="submitted" | .submission_id=$id' "$record" > "$record.new"
  mv "$record.new" "$record"
  printf 'Recorded Apple submission %s (%s).\n' "$submission" "$stage"
}

save_provider_log() {
  # Apple paths refer to submitted bundle members. Keep their warning context,
  # but never persist raw CLI output or unknown top-level provider fields.
  jq -e '
    type == "object" and (.jobId | type == "string") and (.status | type == "string") and
    (.sha256 | type == "string") and
    (.issues == null or (.issues | type == "array")) and
    (.ticketContents == null or (.ticketContents | type == "array"))
  ' "$1" >/dev/null || return 1
  jq '
    with_entries(select(.key == "jobId" or .key == "sha256" or .key == "status" or
      .key == "statusCode" or .key == "statusSummary" or .key == "uploadDate" or
      .key == "logFormatVersion" or .key == "archiveFilename" or .key == "issues" or .key == "ticketContents")) |
    if .archiveFilename then .archiveFilename |= split("/")[-1] else . end |
    if .issues then .issues |= map(with_entries(select(.key == "severity" or .key == "code" or
      .key == "path" or .key == "message" or .key == "docUrl" or .key == "architecture"))) else . end |
    if .ticketContents then .ticketContents |= map(with_entries(select(.key == "path" or .key == "digest" or
      .key == "digestAlgorithm" or .key == "arch"))) else . end
  ' "$1" > "$state/receipts/$stage.log.json.new" || return 1
  mv "$state/receipts/$stage.log.json.new" "$state/receipts/$stage.log.json"
}

finish_stage() {
  [[ -f $record ]] || fail 'No submission exists for this stage.'
  local phase submission response provider_log completed hash wait_result=0 log_result=0
  phase=$(json_field "$record" phase)
  [[ $phase != prepared && $phase != uploading ]] || fail 'No submission ID exists for this prepared stage.'
  [[ $phase != finished ]] || { printf '%s already finished.\n' "$stage"; return; }
  if [[ $phase == submitted ]]; then
    configure_credentials
    submission=$(json_field "$record" submission_id)
    response=$(mktemp)
    provider_log=$(mktemp)
    xcrun notarytool wait "$submission" "${MACOS_NOTARY_AUTH[@]}" --timeout "$NOTARY_TIMEOUT" --output-format json > "$response" || wait_result=$?
    # Rejected submissions and Accepted submissions with warnings both carry
    # useful Apple diagnostics. Attempt the log even after a wait timeout.
    xcrun notarytool log "$submission" "${MACOS_NOTARY_AUTH[@]}" "$provider_log" || log_result=$?
    if [[ $log_result == 0 ]]; then
      save_provider_log "$provider_log" || log_result=1
    fi
    if [[ $wait_result != 0 ]]; then
      rm -f "$response" "$provider_log"
      fail "$stage remains pending or failed; submission $submission is preserved with available Apple diagnostics."
    fi
    if ! jq -e '.status == "Accepted"' "$response" >/dev/null; then
      rm -f "$response" "$provider_log"
      fail "$stage was not Accepted; submission $submission and available Apple diagnostics are preserved."
    fi
    if [[ $log_result != 0 ]]; then
      rm -f "$response" "$provider_log"
      fail 'Cannot retrieve the Apple acceptance log.'
    fi
    hash=$(json_field "$record" archive_sha256)
    if ! jq -e --arg id "$submission" --arg hash "$hash" '
      .status == "Accepted" and .jobId == $id and .sha256 == $hash
    ' "$provider_log" >/dev/null; then
      rm -f "$response" "$provider_log"
      fail 'Apple log does not bind Accepted status to this exact submission ID and archive SHA-256.'
    fi
    if jq -e '.issues != null and (.issues | length > 0)' "$state/receipts/$stage.log.json" >/dev/null; then
      printf 'Apple reported issues for %s; inspect its retained Apple log before release.\n' "$stage" >&2
    fi
    jq '{jobId, sha256, status}' "$provider_log" > "$state/receipts/$stage.json.new"
    mv "$state/receipts/$stage.json.new" "$state/receipts/$stage.json"
    rm -f "$response" "$provider_log"
    atomic_json "$record" '.phase="accepted"'
  fi
  # Recover a crash during stapling from the immutable submitted archive rather
  # than modifying it or applying a new signature.
  rm -rf "$state/work"
  mkdir "$state/work"
  if [[ $stage == dmg ]]; then
    completed="$state/completed/dmg.dmg"
    cp "$submitted_archive" "$state/work/image.dmg"
    macos_staple "$state/work/image.dmg"
    macos_verify_disk_image "$state/work/image.dmg"
    mv "$state/work/image.dmg" "$completed"
  else
    ditto -x -k "$submitted_archive" "$state/work"
    completed="$state/completed/$stage.zip"
    if [[ $stage == payload ]]; then
      app="$state/work/payload/Agent Queue.app"
      macos_staple "$app"
      macos_verify_code "$app"
      macos_assess_app "$app"
      archive "$state/work/payload" "$completed"
    else
      installer="$state/work/Agent Job Queue Installer.app"
      macos_staple "$installer"
      macos_verify_installer_distribution "$installer"
      archive "$installer" "$completed"
    fi
  fi
  jq --arg hash "$(sha256 "$completed")" '.phase="finished" | .completed_sha256=$hash' "$record" > "$record.new"
  mv "$record.new" "$record"
  rm -rf "$state/work"
}

export_assets() {
  local stage destination=$1 base="Agent-Job-Queue-Installer-$resume_version-universal"
  for stage in payload installer dmg; do
    [[ -f $state/stages/$stage.json && $(json_field "$state/stages/$stage.json" phase) == finished ]] ||
      fail 'All three stages must be Accepted and finished before publishing.'
  done
  destination=$(macos_signing_canonical_path "$destination")
  case "$destination/" in "$state/"*) fail 'Public assets must be outside private working state.' ;; esac
  mkdir -p "$destination"
  [[ -z $(find "$destination" -mindepth 1 -maxdepth 1 -print -quit) ]] || fail 'Artifact destination must be empty.'
  # Reverify tickets and signatures on the final immutable assets before export.
  rm -rf "$state/work"
  mkdir "$state/work"
  ditto -x -k "$state/completed/installer.zip" "$state/work"
  macos_verify_installer_distribution "$state/work/Agent Job Queue Installer.app"
  macos_verify_disk_image "$state/completed/dmg.dmg"
  cp "$state/completed/installer.zip" "$destination/$base.zip"
  cp "$state/completed/dmg.dmg" "$destination/$base.dmg"
  (cd "$destination" && shasum -a 256 "$base.zip" > "$base.zip.sha256" && shasum -a 256 "$base.dmg" > "$base.dmg.sha256")
  rm -rf "$state/work"
}

command=${1:-}
# Explicit version arguments ensure restored metadata cannot choose a release.
case "$command" in
  init)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh init INSTALLER STATE VERSION'
    initial_installer=$2 state=$3 resume_version=$4
    ;;
  verify-state)
    [[ $# == 3 ]] || fail 'Usage: macos-resume.sh verify-state STATE VERSION'
    state=$2 resume_version=$3
    ;;
  prepare|submit|finish)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh {prepare|submit|finish} STATE STAGE VERSION'
    state=$2 stage=$3 resume_version=$4
    ;;
  export)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh export STATE DESTINATION VERSION'
    state=$2 destination=$3 resume_version=$4
    ;;
  *) fail 'Usage: macos-resume.sh {init|verify-state|prepare|submit|finish|export} ...' ;;
esac
identity
state=$(macos_signing_canonical_path "$state")
if [[ $command == init ]]; then
  [[ ! -e $state && -d $initial_installer ]] || fail 'Init requires a complete installer and a fresh state directory.'
  initial_installer=$(macos_signing_canonical_path "$initial_installer")
  case "$state/" in "$initial_installer/"*) fail 'State must be outside the initial installer.' ;; esac
  mkdir "$state"
  lock_state
  mkdir "$state/archives" "$state/completed" "$state/stages" "$state/receipts" "$state/work"
  ditto "$initial_installer" "$state/work/Agent Job Queue Installer.app"
  for path in 'Agent Queue.app/Contents/MacOS/AgentQueue' source/bin/agent-heavy source/bin/agent-remote; do
    [[ -f "$state/work/Agent Job Queue Installer.app/Contents/Resources/payload/$path" ]] || fail 'The complete installer payload is required.'
  done
  archive "$state/work/Agent Job Queue Installer.app" "$state/initial.zip"
  jq -n --arg version "$resume_version" --arg team "$APPLE_TEAM_ID" --arg sha "$GITHUB_SHA" \
    --arg repository "$GITHUB_REPOSITORY" --arg run "$GITHUB_RUN_ID" --arg number "$GITHUB_RUN_NUMBER" \
    --arg hash "$(sha256 "$state/initial.zip")" '
    {schema:1, version:$version, team:$team, source_sha:$sha, repository:$repository, run_id:$run, run_number:$number, initial_sha256:$hash}
  ' > "$state/metadata.json"
  rm -rf "$state/work"
else
  lock_state
  verify_state
  case "$command" in
    verify-state) ;;
    prepare) require_stage; prepare_stage ;;
    submit) require_stage; submit_stage ;;
    finish) require_stage; finish_stage ;;
    export) export_assets "$destination" ;;
  esac
  verify_state
fi
