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
  source_sha=${BUILD_SHA:-$GITHUB_SHA}
  source_run=${BUILD_RUN_ID:-$GITHUB_RUN_ID}
  source_number=${BUILD_RUN_NUMBER:-$GITHUB_RUN_NUMBER}
  [[ $source_sha =~ $sha_pattern && $source_run =~ $number_pattern && $source_number =~ $number_pattern ]] || fail 'Invalid original build identity.'
  if [[ -n ${BUILD_RUN_ID:-} ]]; then
    [[ ${BUILD_RUN_ATTEMPT:-} =~ $number_pattern && ${BUILD_ARTIFACT_ID:-} =~ $number_pattern &&
      ${BUILD_ARTIFACT_DIGEST:-} =~ ^sha256:[a-f0-9]{64}$ && ${BUILD_VERSION:-} == "$resume_version" ]] ||
      fail 'Exact original source artifact identity and version are required.'
  fi
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
  check_log_name "$log" "$stage"
}

check_log_name() {
  local log=$1 selected_stage=$2 expected legacy
  expected=$(macos_notary_archive_name "$selected_stage" "$resume_version")
  # Older, fully bound checkpoints used version-only names. Their UUID/hash and
  # original-artifact checks remain mandatory when current automation resumes.
  legacy=$(BUILD_RUN_ID='' macos_notary_archive_name "$selected_stage" "$resume_version")
  jq -e --arg expected "$expected" --arg legacy "$legacy" '
    (has("archiveFilename") | not) or .archiveFilename == $expected or .archiveFilename == $legacy
  ' "$log" >/dev/null || fail 'Apple log archive filename does not match this product, version and stage.'
}

verify_state() {
  local metadata="$state/metadata.json" stage phase previous=finished record hash
  [[ -f $metadata && ! -L $metadata ]] || fail 'State metadata is missing.'
  jq -e --arg version "$resume_version" --arg team "$APPLE_TEAM_ID" --arg sha "$source_sha" \
    --arg repository "$GITHUB_REPOSITORY" --arg run "$source_run" --arg number "$source_number" \
    --arg attempt "${BUILD_RUN_ATTEMPT:-}" --arg artifact "${BUILD_ARTIFACT_ID:-}" --arg digest "${BUILD_ARTIFACT_DIGEST:-}" '
    (if .schema == 1 then
      keys == ["initial_sha256", "repository", "run_id", "run_number", "schema", "source_sha", "team", "version"]
    else .schema == 2 and
      keys == ["artifact_digest", "artifact_id", "build_attempt", "initial_sha256", "repository", "run_id", "run_number", "schema", "source_sha", "team", "version"] and
      .artifact_id == $artifact and .artifact_digest == $digest and .build_attempt == $attempt end) and
    .version == $version and .team == $team and .source_sha == $sha and
    .repository == $repository and .run_id == $run and .run_number == $number and
    (.initial_sha256 | type == "string" and test("^[a-f0-9]{64}$"))
  ' "$metadata" >/dev/null || fail 'State metadata does not match this release, team, repository, commit and run.'
  local directory
  for directory in archives completed stages receipts; do
    [[ -d $state/$directory && ! -L $state/$directory ]] || fail 'An expected state directory is missing or is a symlink.'
  done
  check_file "$state/initial.zip" "$(json_field "$metadata" initial_sha256)"
  # Checkpoints from the installer-era layout cannot be continued: they would
  # restart as an app stage over a different initial archive.
  [[ ! -e $state/stages/payload.json && ! -e $state/stages/installer.json ]] ||
    fail 'State predates the drag-and-drop app layout and cannot be resumed.'
  for stage in app dmg; do
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
      ((keys == ["archive_sha256", "completed_sha256", "phase", "prepared_attempt", "prepared_run_id", "stage", "submission_id"] and
        (.prepared_run_id | type == "string" and test("^[1-9][0-9]*$"))) or
       keys == ["archive_sha256", "completed_sha256", "phase", "prepared_attempt", "stage", "submission_id"]) and
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
  if [[ -e $state/publication.json || -L $state/publication.json ]]; then verify_publication; fi
}

verify_publication() {
  [[ -f $state/publication.json && ! -L $state/publication.json ]] || fail 'Retained publication evidence is unsafe.'
  jq -s -e --slurpfile metadata "$state/metadata.json" \
    --slurpfile app "$state/stages/app.json" --slurpfile dmg "$state/stages/dmg.json" \
    --slurpfile app_log "$state/receipts/app.log.json" --slurpfile dmg_log "$state/receipts/dmg.log.json" '
    def toolchain_valid:
      keys == ["automation_sha","go","host_architecture","macos_sdk","notarytool","os",
        "runner_image","runner_image_version","schema","source_sha","swift","xcode","xcode_build"] and
      all(to_entries[]; if .key == "schema" then .value == 1
        else (.value | type == "string" and length > 0) end) and
      (.host_architecture == "arm64" or .host_architecture == "x86_64");
    def evidence_valid:
      keys == ["automation_sha","execution_attempt","execution_id","logs","metadata","schema","stages","toolchain"] and
    .schema == 1 and .metadata == $metadata[0] and
    .stages == [$app[0],$dmg[0]] and
    all(.stages[]; .phase == "finished") and
    .logs == [$app_log[0],$dmg_log[0]] and
    (.execution_id | type == "string" and test("^[1-9][0-9]*$")) and
    (.execution_attempt | type == "string" and test("^[1-9][0-9]*$")) and
    (.automation_sha | type == "string" and test("^[a-f0-9]{40}$")) and
    (.toolchain | toolchain_valid) and .toolchain.source_sha == .metadata.source_sha and
    .toolchain.automation_sha == .automation_sha;
    ([$metadata,$app,$dmg,$app_log,$dmg_log] |
      all(.[]; length == 1 and (.[0] | type == "object"))) and
    length == 1 and all(.[]; type == "object" and evidence_valid)
  ' "$state/publication.json" >/dev/null || fail 'Retained publication evidence does not match this exact completed candidate.'
  # Only the producer's sorted serialization is retained. Parsing alone would
  # discard earlier duplicate keys, leaving hidden bytes in the public asset.
  cmp -s "$state/publication.json" <(jq -S . "$state/publication.json") ||
    fail 'Retained publication evidence is not canonical JSON.'
}

publication_evidence() {
  [[ -n ${BUILD_RUN_ID:-} && $(json_field "$state/metadata.json" schema) == 2 ]] ||
    fail 'Publication evidence requires an artifact-bound CI candidate.'
  local stage
  for stage in app dmg; do
    [[ -f $state/stages/$stage.json && $(json_field "$state/stages/$stage.json" phase) == finished ]] ||
      fail 'All stages must finish before retaining publication evidence.'
  done
  if [[ ! -e $state/publication.json && ! -L $state/publication.json ]]; then
    [[ -f ${APPLE_TOOLCHAIN_RECEIPT:-} && ! -L ${APPLE_TOOLCHAIN_RECEIPT:-} ]] || fail 'The finalization toolchain receipt is required.'
    jq -s -e 'length == 1 and (.[0] | type == "object")' "$APPLE_TOOLCHAIN_RECEIPT" >/dev/null ||
      fail 'The finalization toolchain receipt must be one JSON object.'
    jq -S -n --slurpfile metadata "$state/metadata.json" \
      --slurpfile app "$state/stages/app.json" --slurpfile dmg "$state/stages/dmg.json" \
      --slurpfile app_log "$state/receipts/app.log.json" --slurpfile dmg_log "$state/receipts/dmg.log.json" \
      --slurpfile toolchain "$APPLE_TOOLCHAIN_RECEIPT" --arg execution "$GITHUB_RUN_ID" \
      --arg attempt "$GITHUB_RUN_ATTEMPT" --arg automation "$GITHUB_SHA" '
      {schema:1,metadata:$metadata[0],stages:[$app[0],$dmg[0]],
       logs:[$app_log[0],$dmg_log[0]],toolchain:$toolchain[0],
       execution_id:$execution,execution_attempt:$attempt,automation_sha:$automation}
    ' > "$state/publication.json.new"
    mv "$state/publication.json.new" "$state/publication.json"
  fi
  verify_publication
}

configure_credentials() {
  SIGNING_MODE=developer-id
  NOTARY_TIMEOUT=${NOTARY_TIMEOUT:-5m}
  macos_signing_configure || return
  MACOS_NOTARY_REST=0
  if [[ -n ${NOTARY_KEY_FILE:-} ]]; then
    if [[ ${QUEUE_NOTARY_TOOL:-} != /* || ! -f ${QUEUE_NOTARY_TOOL:-} || ! -x ${QUEUE_NOTARY_TOOL:-} ]]; then
      macos_signing_error 'Raw API keys require an executable prebuilt notarization client in QUEUE_NOTARY_TOOL.'
      return 1
    fi
    # Native notarytool accepts bare seconds. Normalize only the REST path and
    # reject its unsupported wait budget before signing or creating a submission.
    local timeout_amount=${NOTARY_TIMEOUT%[smh]} timeout_unit=${NOTARY_TIMEOUT##*[0-9]} timeout_max
    case "$timeout_unit" in
      ''|s) timeout_max=86400 ;;
      m) timeout_max=1440 ;;
      h) timeout_max=24 ;;
    esac
    # The length guard short-circuits before arithmetic on oversized integers.
    if (( ${#timeout_amount} > ${#timeout_max} )) || (( timeout_amount > timeout_max )); then
      macos_signing_error 'Raw API notarization timeout must be at most 24h.'
      return 1
    fi
    [[ -n $timeout_unit ]] || NOTARY_TIMEOUT="${timeout_amount}s"
    MACOS_NOTARY_REST=1
  fi
  local name path
  for name in APPLE_SIGNING_KEYCHAIN NOTARY_KEYCHAIN NOTARY_KEY_FILE; do
    [[ -n ${!name:-} ]] || continue
    path=$(macos_signing_canonical_path "${!name}") || return
    case "$path/" in
      "$state/"*) macos_signing_error 'Signing credentials must remain outside resumable state.'; return 1 ;;
    esac
  done
}

require_stage() {
  case "$stage" in app|dmg) ;; *) fail 'Stage must be app or dmg.' ;; esac
  record="$state/stages/$stage.json"
  if [[ $stage == dmg ]]; then submitted_archive="$state/archives/dmg.dmg"; else submitted_archive="$state/archives/$stage.zip"; fi
}

restore_app() {
  rm -rf "$state/work" || return
  mkdir "$state/work" || return
  ditto -x -k "$state/initial.zip" "$state/work" || return
  app="$state/work/Agent Queue.app"
  [[ -f $app/Contents/MacOS/agent-heavy ]] || { macos_signing_error 'The initial archive is not a self-contained Agent Queue app.'; return 1; }
}

prepare_stage() {
  local image_signing_arguments
  [[ ! -f $record ]] || { printf '%s already prepared.\n' "$stage"; return; }
  if [[ $stage == dmg ]]; then
    [[ -f $state/stages/app.json && $(json_field "$state/stages/app.json" phase) == finished ]] ||
      fail 'The previous stage must be accepted and finished before preparing this stage.'
  fi
  macos_signing_operation configure-credentials configure_credentials
  if [[ $MACOS_NOTARY_REST == 1 ]]; then
    macos_signing_operation notary-authentication "$QUEUE_NOTARY_TOOL" notarize preflight
  fi
  macos_signing_operation restore-app restore_app
  case "$stage" in
    app)
      macos_sign_app_helpers "$app"
      macos_sign_code "$app" '' app
      archive "$app" "$submitted_archive"
      ;;
    dmg)
      rm -rf "$app"
      ditto -x -k "$state/completed/app.zip" "$state/work"
      macos_verify_app_distribution "$app"
      macos_disk_image_source "$app" "$state/work/image"
      sync
      hdiutil create -verbose -nospotlight -fs HFS+ -volname 'Agent Queue' \
        -srcfolder "$MACOS_DISK_IMAGE_SOURCE" -ov -format UDZO "$submitted_archive"
      image_signing_arguments=(--force --sign "$APPLE_SIGNING_IDENTITY" --timestamp --identifier io.went.agent-queue.disk-image)
      [[ -z ${APPLE_SIGNING_KEYCHAIN:-} ]] || image_signing_arguments+=(--keychain "$APPLE_SIGNING_KEYCHAIN")
      macos_signing_operation sign-dmg codesign "${image_signing_arguments[@]}" "$submitted_archive"
      macos_signing_operation verify-dmg macos_verify_disk_image_code "$submitted_archive"
      ;;
  esac
  jq -n --arg stage "$stage" --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" --arg hash "$(sha256 "$submitted_archive")" '
    {stage:$stage, phase:"prepared", prepared_run_id:$run, prepared_attempt:$attempt, archive_sha256:$hash, submission_id:"", completed_sha256:""}
  ' > "$record.new"
  mv "$record.new" "$record"
  rm -rf "$state/work"
}

submit_stage() {
  [[ -f $record ]] || fail 'Prepare this stage before submitting it.'
  [[ $(json_field "$record" phase) != uploading ]] || fail 'A previous upload has an unknown outcome; manual reconciliation is required, never resubmit.'
  [[ $(json_field "$record" phase) == prepared ]] || { printf '%s submission already recorded.\n' "$stage"; return; }
  # Legacy records were prepared by the source CI execution. Attempt numbers
  # alone are not identities: every new distribution execution starts at one.
  local prepared_run
  prepared_run=$(jq -er --arg legacy "$source_run" '.prepared_run_id // $legacy' "$record")
  [[ $prepared_run == "$GITHUB_RUN_ID" && $(json_field "$record" prepared_attempt) == "$GITHUB_RUN_ATTEMPT" ]] ||
    fail 'Prepared snapshot has no submission ID from an earlier attempt. Upload outcome is unknown; reconcile with Apple manually, never resubmit.'
  configure_credentials
  # Keep checkpoint paths stable, but give Apple a recognizable submission name.
  # Copy the immutable bytes rather than rebuilding or changing signed contents.
  local upload_archive
  upload_archive="$state/work/$(macos_notary_archive_name "$stage" "$resume_version")"
  rm -rf "$state/work"
  mkdir "$state/work"
  cp "$submitted_archive" "$upload_archive"
  check_file "$upload_archive" "$(json_field "$record" archive_sha256)"
  if [[ $MACOS_NOTARY_REST == 1 ]]; then
    # The client preflights authentication before recording upload intent, then
    # durably records Apple's ID before S3. Never premark or rewrite its record.
    macos_signing_operation notary-submit "$QUEUE_NOTARY_TOOL" notarize submit \
      --archive "$upload_archive" --record "$record" --sha256 "$(json_field "$record" archive_sha256)"
    return
  fi
  local response submission id_pattern='^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$'
  response=$(mktemp)
  # Raw provider output stays outside the durable public state. Do not retain
  # stderr, auth arguments or private runner paths in snapshots.
  atomic_json "$record" '.phase="uploading"'
  if ! xcrun notarytool submit "$upload_archive" "${MACOS_NOTARY_AUTH[@]}" --output-format json --no-progress > "$response"; then
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
  # An earlier transient must never authorize a retry of a later terminal error.
  rm -f "$state/receipts/$stage.retry" "$state/receipts/$stage.status.json"
  MACOS_TICKET_RECOVERY_FILE="$state/receipts/$stage.retry"
  if [[ $phase == submitted ]]; then
    configure_credentials
    submission=$(json_field "$record" submission_id)
    response=$(mktemp)
    provider_log=$(mktemp)
    if [[ $MACOS_NOTARY_REST == 1 ]]; then
      "$QUEUE_NOTARY_TOOL" notarize wait --id "$submission" --timeout "$NOTARY_TIMEOUT" > "$response" || wait_result=$?
    else
      xcrun notarytool wait "$submission" "${MACOS_NOTARY_AUTH[@]}" --timeout "$NOTARY_TIMEOUT" --output-format json > "$response" || wait_result=$?
    fi
    # Retain only allowlisted, UUID-bound provider status. Unknown/malformed
    # responses provide no authority for the automatic observer to retry.
    if jq -e --arg id "$submission" '
      .id == $id and (.status == "In Progress" or .status == "Accepted" or
        .status == "Invalid" or .status == "Rejected")
    ' "$response" >/dev/null 2>&1; then
      jq --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" \
        '{id, status} + (if .error == "timeout" then {error:"timeout"} else {} end) + {execution_id:$run,execution_attempt:$attempt}' \
        "$response" > "$state/receipts/$stage.status.json"
    fi
    # Rejected submissions and Accepted submissions with warnings both carry
    # useful Apple diagnostics. Attempt the log even after a wait timeout.
    if [[ $MACOS_NOTARY_REST == 1 ]]; then
      # Let the REST client create its private output, without depending on
      # native notarytool's behavior of overwriting the mktemp placeholder.
      rm "$provider_log"
      "$QUEUE_NOTARY_TOOL" notarize log --id "$submission" --output "$provider_log" || log_result=$?
    else
      xcrun notarytool log "$submission" "${MACOS_NOTARY_AUTH[@]}" "$provider_log" || log_result=$?
    fi
    if [[ $log_result == 0 ]]; then
      save_provider_log "$provider_log" || log_result=1
    fi
    if [[ $wait_result != 0 ]]; then
      rm -f "$response" "$provider_log"
      fail "$stage remains pending or failed; submission $submission is preserved with available Apple diagnostics."
    fi
    if ! jq -e --arg id "$submission" --arg rest "$MACOS_NOTARY_REST" \
      '.status == "Accepted" and ($rest != "1" or .id == $id) and ((has("id") | not) or .id == $id)' "$response" >/dev/null; then
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
    check_log_name "$provider_log" "$stage"
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
    completed="$state/completed/app.zip"
    app="$state/work/Agent Queue.app"
    macos_staple "$app"
    macos_verify_app_distribution "$app"
    archive "$app" "$completed"
  fi
  jq --arg hash "$(sha256 "$completed")" '.phase="finished" | .completed_sha256=$hash' "$record" > "$record.new"
  mv "$record.new" "$record"
  rm -f "$state/receipts/$stage.retry"
  rm -rf "$state/work"
}

export_assets() {
  local stage destination=$1 base="Agent-Queue-$resume_version-universal"
  rm -f "$state/receipts/dmg.retry"
  MACOS_TICKET_RECOVERY_FILE="$state/receipts/dmg.retry"
  for stage in app dmg; do
    [[ -f $state/stages/$stage.json && $(json_field "$state/stages/$stage.json" phase) == finished ]] ||
      fail 'Both stages must be Accepted and finished before publishing.'
  done
  destination=$(macos_signing_canonical_path "$destination")
  case "$destination/" in "$state/"*) fail 'Public assets must be outside private working state.' ;; esac
  mkdir -p "$destination"
  [[ -z $(find "$destination" -mindepth 1 -maxdepth 1 -print -quit) ]] || fail 'Artifact destination must be empty.'
  # Reverify tickets and signatures on the final immutable assets before export.
  rm -rf "$state/work"
  mkdir "$state/work"
  ditto -x -k "$state/completed/app.zip" "$state/work"
  macos_verify_app_distribution "$state/work/Agent Queue.app"
  macos_verify_disk_image "$state/completed/dmg.dmg"
  cp "$state/completed/app.zip" "$destination/$base.zip"
  cp "$state/completed/dmg.dmg" "$destination/$base.dmg"
  (cd "$destination" && shasum -a 256 "$base.zip" > "$base.zip.sha256" && shasum -a 256 "$base.dmg" > "$base.dmg.sha256")
  rm -rf "$state/work"
}

bind_origin() {
  [[ -n ${BUILD_RUN_ID:-} && ${QUEUE_NOTARY_TOOL:-} == /* && -x ${QUEUE_NOTARY_TOOL:-} ]] ||
    fail 'Original artifact binding requires verified build context and the prebuilt trusted verifier.'
  "$QUEUE_NOTARY_TOOL" verify-checkpoint-origin --original "$original_archive" --initial "$state/initial.zip"
  if [[ $(json_field "$state/metadata.json" schema) == 1 ]]; then
    # Metadata upgrade follows exact original-byte/mode/resource verification.
    # The submitted archives, UUIDs and Apple receipts are never rewritten.
    jq --arg attempt "$BUILD_RUN_ATTEMPT" --arg artifact "$BUILD_ARTIFACT_ID" --arg digest "$BUILD_ARTIFACT_DIGEST" '
      . + {schema:2,build_attempt:$attempt,artifact_id:$artifact,artifact_digest:$digest}
    ' "$state/metadata.json" > "$state/metadata.json.new"
    mv "$state/metadata.json.new" "$state/metadata.json"
  fi
}

command=${1:-}
# Explicit version arguments ensure restored metadata cannot choose a release.
case "$command" in
  init)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh init APP STATE VERSION'
    initial_app=$2 state=$3 resume_version=$4
    ;;
  verify-state|evidence)
    [[ $# == 3 ]] || fail "Usage: macos-resume.sh $command STATE VERSION"
    state=$2 resume_version=$3
    ;;
  begin|prepare|submit|finish)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh {begin|prepare|submit|finish} STATE STAGE VERSION'
    state=$2 stage=$3 resume_version=$4
    ;;
  export)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh export STATE DESTINATION VERSION'
    state=$2 destination=$3 resume_version=$4
    ;;
  bind-origin)
    [[ $# == 4 ]] || fail 'Usage: macos-resume.sh bind-origin STATE ORIGINAL_ARCHIVE VERSION'
    state=$2 original_archive=$3 resume_version=$4
    ;;
  *) fail 'Usage: macos-resume.sh {init|verify-state|bind-origin|evidence|begin|prepare|submit|finish|export} ...' ;;
esac
identity
state=$(macos_signing_canonical_path "$state")
if [[ $command == init ]]; then
  [[ ! -e $state && -d $initial_app ]] || fail 'Init requires a complete app and a fresh state directory.'
  initial_app=$(macos_signing_canonical_path "$initial_app")
  case "$state/" in "$initial_app/"*) fail 'State must be outside the initial app.' ;; esac
  mkdir "$state"
  lock_state
  mkdir "$state/archives" "$state/completed" "$state/stages" "$state/receipts" "$state/work"
  ditto "$initial_app" "$state/work/Agent Queue.app"
  for path in AgentQueue agent-heavy agent-remote; do
    [[ -f "$state/work/Agent Queue.app/Contents/MacOS/$path" ]] || fail 'The complete app and its bundled helpers are required.'
  done
  macos_app_payload_mode "$state/work/Agent Queue.app"
  [[ $MACOS_APP_MANAGED == 1 && $MACOS_APP_EMBEDDED == 1 ]] ||
    fail 'The app must be the self-contained layout; installer-era builds are not supported.'
  archive "$state/work/Agent Queue.app" "$state/initial.zip"
  jq -n --arg version "$resume_version" --arg team "$APPLE_TEAM_ID" --arg sha "$source_sha" \
    --arg repository "$GITHUB_REPOSITORY" --arg run "$source_run" --arg number "$source_number" \
    --arg attempt "${BUILD_RUN_ATTEMPT:-}" --arg artifact "${BUILD_ARTIFACT_ID:-}" --arg digest "${BUILD_ARTIFACT_DIGEST:-}" \
    --arg hash "$(sha256 "$state/initial.zip")" '
    {schema:1, version:$version, team:$team, source_sha:$sha, repository:$repository, run_id:$run, run_number:$number, initial_sha256:$hash} |
    if $artifact != "" then . + {schema:2, build_attempt:$attempt, artifact_id:$artifact, artifact_digest:$digest} else . end
  ' > "$state/metadata.json"
  rm -rf "$state/work"
else
  lock_state
  verify_state
  case "$command" in
    verify-state) ;;
    begin) require_stage; rm -f "$state/receipts/$stage.retry" "$state/receipts/$stage.status.json" ;;
    prepare) require_stage; prepare_stage ;;
    submit) require_stage; submit_stage ;;
    finish) require_stage; finish_stage ;;
    export) export_assets "$destination" ;;
    bind-origin) bind_origin ;;
    evidence) publication_evidence ;;
  esac
  verify_state
fi
