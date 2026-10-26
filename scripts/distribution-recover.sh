#!/bin/bash
# Observe trusted main executions. The only mutation is a same-run failed-job retry.
set -euo pipefail
umask 077
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/distribution-common.sh
source "$root/scripts/distribution-common.sh"

[[ $# == 0 && ${GITHUB_ACTIONS:-} == true && ${GITHUB_REF:-} == refs/heads/main &&
  ${GITHUB_REPOSITORY:-} =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_.-]{1,100}$ &&
  ${GITHUB_REPOSITORY##*/} != . && ${GITHUB_REPOSITORY##*/} != .. ]] ||
  distribution_error 'Recovery requires a trusted same-repository main observer with no arguments.'
case "${GITHUB_EVENT_NAME:-}" in workflow_run|schedule|workflow_dispatch) ;;
  *) distribution_error 'Unexpected recovery observer event.' ;; esac

temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

recovery_runs() {
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/workflows/distribution.yml/runs?branch=main" workflow_runs "$1.raw"
  jq -s --arg repo "$GITHUB_REPOSITORY" '[.[] | select(
    .path == ".github/workflows/distribution.yml" and .head_branch == "main" and
    (.event == "workflow_run" or .event == "workflow_dispatch") and
    .repository.full_name == $repo and .head_repository.full_name == $repo and
    (.display_title|type == "string" and test("^Distribute build [1-9][0-9]{0,14}$")))]' "$1.raw" > "$1"
}

# Validate the complete flat envelope before reading any JSON. No ZIP entries
# are written to disk, and both compressed and expanded sizes are bounded.
recovery_archive() {
  local archive=$1 entry count expanded=0 size
  (( $(wc -c < "$archive") <= 1048576 )) || distribution_error 'Receipt ZIP exceeds 1 MiB.'
  unzip -Z1 "$archive" > "$temporary/names" || distribution_error 'Unreadable receipt ZIP.'
  unzip -Z -l "$archive" > "$temporary/modes" || distribution_error 'Unreadable receipt ZIP metadata.'
  count=$(wc -l < "$temporary/names" | tr -d ' ')
  ((count > 0 && count <= 15)) || distribution_error 'Unexpected receipt ZIP entry count.'
  [[ $(LC_ALL=C sort "$temporary/names" | uniq -d | wc -l | tr -d ' ') == 0 ]] || distribution_error 'Duplicate receipt ZIP entry.'
  while IFS= read -r entry; do
    case "$entry" in recovery.json|metadata.json|toolchain.json) ;;
      stage-payload.json|stage-installer.json|stage-dmg.json|receipt-payload.json|receipt-installer.json|receipt-dmg.json|log-payload.json|log-installer.json|log-dmg.json|status-payload.json|status-installer.json|status-dmg.json) ;;
      *) distribution_error 'Unsafe or unexpected receipt ZIP entry.' ;; esac
  done < "$temporary/names"
  # Info-ZIP lists mode, version, host, expanded size, flags, compressed size,
  # method, date, time and name. Reject directories, links, special files,
  # encryption and ambiguous metadata, including newline-bearing names.
  awk 'length($1) == 10 {if ($1 !~ /^-/ || NF != 10 || $4 !~ /^[0-9]+$/ ||
      $5 !~ /^[bt][-lxX]$/ || $6 !~ /^[0-9]+$/) bad=1; size += $4; print $10}
    END {if (bad || size > 1048576) exit 1}' "$temporary/modes" > "$temporary/regular" ||
    distribution_error 'Unsafe receipt ZIP type, flags or expanded size.'
  if [[ $(wc -l < "$temporary/regular" | tr -d ' ') != "$count" ]] ||
    ! cmp -s "$temporary/names" "$temporary/regular"; then distribution_error 'Ambiguous receipt ZIP entries.'; fi
  for entry in recovery.json metadata.json; do
    grep -qx "$entry" "$temporary/names" || distribution_error "Missing $entry in receipt ZIP."
  done
  # JSON is read via unzip -p only after every entry has been validated.
  : > "$temporary/documents"
  while IFS= read -r entry; do
    # Bound actual output too, rather than trusting only central-directory
    # lengths. unzip verifies each entry CRC while streaming it.
    unzip -p "$archive" "$entry" | head -c 1048577 > "$temporary/document.json" || distribution_error "Corrupt or oversized $entry."
    size=$(wc -c < "$temporary/document.json")
    expanded=$((expanded + size))
    ((expanded <= 1048576)) || distribution_error 'Expanded receipt ZIP exceeds 1 MiB.'
    jq -e -s 'length == 1 and (.[0]|type) == "object"' "$temporary/document.json" >/dev/null || distribution_error "Malformed $entry."
    jq -c --arg name "$entry" '{key:$name,value:.}' "$temporary/document.json" >> "$temporary/documents"
  done < "$temporary/names"
  jq -s 'from_entries' "$temporary/documents" > "$temporary/envelope.json"
  jq '."recovery.json"' "$temporary/envelope.json" > "$temporary/recovery.json"
}

recovery_candidate() (
  local candidate=$1 id build attempt sha title receipt slot digest checkpoint source_attempt artifact mode receipt_attempt
  id=$(jq -r '.id' <<< "$candidate")
  title=$(jq -r '.display_title' <<< "$candidate")
  build=${title#Distribute build }
  gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id" > "$temporary/execution.json" || distribution_error 'Cannot read execution.'
  jq -e --argjson expected "$candidate" '
    . as $run | ["id","path","event","head_branch","head_sha","run_attempt","display_title","status","conclusion","created_at","updated_at","repository","head_repository"] |
    all(.[]; . as $key | $run[$key] == $expected[$key])' "$temporary/execution.json" >/dev/null ||
    distribution_error 'Execution changed during observation.'
  attempt=$(jq -r '.run_attempt' "$temporary/execution.json")
  sha=$(jq -r '.head_sha' "$temporary/execution.json")
  if ! distribution_number "$attempt" || ((attempt >= 48)); then distribution_error 'Automatic attempt limit reached (maximum 48).'; fi
  jq -e '(.created_at|fromdateiso8601) as $created |
    (.head_sha|type == "string" and test("^[a-f0-9]{40}$")) and
    .status == "completed" and .conclusion == "failure" and
    $created <= now and now - $created <= 604800' \
    "$temporary/execution.json" >/dev/null || distribution_error 'Run is not a recent completed failure (maximum age 7 days).'
  # The first continuation can run immediately after a finite Apple wait. Later
  # completion events must yield to the scheduled observer instead of consuming
  # the whole budget in a tight chain of provider failures.
  jq -e --argjson attempt "$attempt" '
    (.updated_at|fromdateiso8601) as $updated | (.created_at|fromdateiso8601) as $created |
    $updated >= $created and $updated <= now and ($attempt == 1 or now - $updated >= 900)
  ' "$temporary/execution.json" >/dev/null || distribution_error 'Continuation cooldown has not elapsed (15 minutes after later attempts).'

  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$id/jobs?filter=latest" jobs "$temporary/jobs"
  mode=$(jq -s -er --arg id "$id" --arg sha "$sha" --argjson attempt "$attempt" '
    def signing: .name == "Sign and notarize main installer";
    def delivery: .name == "Publish main installer" or .name == "Verify Homebrew delivery";
    def success: .status == "completed" and .conclusion == "success";
    if all(.[]; (.run_id|tostring) == $id and .head_sha == $sha and (.run_attempt|type) == "number" and
      .run_attempt >= 1 and .run_attempt <= $attempt and .run_attempt == (.run_attempt|floor) and .status == "completed") and
      ([.[].name]|length) == ([.[].name]|unique|length) and ([.[]|select(signing)]|length) == 1 then
      if any(.[]; signing and .conclusion == "failure" and .run_attempt == $attempt) and
        all(.[]; signing or success or .conclusion == "skipped") then "signing"
      elif any(.[]; signing and success) and any(.[]; delivery and .conclusion == "failure" and .run_attempt == $attempt) and
        all(.[]; success or (delivery and (.conclusion == "failure" or .conclusion == "skipped"))) and
        ([.[]|select((.name == "Notarized distribution (macos-15)" or .name == "Notarized distribution (macos-15-intel)") and success)]|length) == 2 then "delivery"
      else empty end
    else empty end' "$temporary/jobs") || distribution_error 'Failure is neither a signing continuation nor delivery-only after both acceptance jobs.'

  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$id/artifacts" artifacts "$temporary/artifacts"
  jq -s -e --arg prefix "Apple-receipts-b$build-r$id-a" --argjson attempt "$attempt" '
    [.[]|select(.name|startswith("Apple-receipts-"))] |
    ([.[].name]|length) == ([.[].name]|unique|length) and all(.[];
      (.name|test("^" + $prefix + "[1-9][0-9]{0,14}-s(0[0-9]|10)$")) and
      (.name|capture("-a(?<attempt>[0-9]+)-s").attempt|tonumber) <= $attempt)' "$temporary/artifacts" >/dev/null ||
    distribution_error 'Mixed or malformed receipt slots.'
  # Delivery-only reruns retain successful signing from earlier attempts. Their
  # newest feature-era receipt still binds those exact bytes. Failed signing
  # must instead provide fresh evidence from the currently failed attempt.
  receipt=$(jq -s -c --arg mode "$mode" --argjson attempt "$attempt" '
    [.[]|select(.name|startswith("Apple-receipts-"))|
      . + (.name|capture("-a(?<attempt>[0-9]+)-s(?<slot>[0-9]+)$")) |
      .attempt |= tonumber | .slot |= tonumber | select($mode == "delivery" or .attempt == $attempt)] |
    sort_by(.attempt,.slot)|last // empty' "$temporary/artifacts")
  [[ -n $receipt ]] || distribution_error 'No applicable recovery receipt; manual investigation required.'
  receipt_attempt=$(jq -r '.attempt' <<< "$receipt")
  slot=$(jq -r '.slot' <<< "$receipt")
  jq -e --arg id "$id" --arg sha "$sha" '
    .expired == false and (.digest|type == "string" and test("^sha256:[a-f0-9]{64}$")) and
    (.workflow_run.id|tostring) == $id and .workflow_run.head_branch == "main" and .workflow_run.head_sha == $sha
  ' <<< "$receipt" >/dev/null || distribution_error 'Newest receipt expired or has unbound provenance/digest.'
  artifact=$(jq -r '.id' <<< "$receipt")
  digest=$(jq -r '.digest' <<< "$receipt")
  gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact/zip" > "$temporary/receipt.zip" || distribution_error 'Cannot download receipt ZIP.'
  [[ sha256:$(shasum -a 256 "$temporary/receipt.zip" | awk '{print $1}') == "$digest" ]] || distribution_error 'Receipt archive digest mismatch.'
  recovery_archive "$temporary/receipt.zip"

  jq -e --arg repo "$GITHUB_REPOSITORY" --arg build "$build" --arg id "$id" --arg attempt "$receipt_attempt" --arg sha "$sha" --arg mode "$mode" '
    def number: test("^[1-9][0-9]{0,14}$");
    def hash: test("^[a-f0-9]{64}$");
    def digest: test("^sha256:[a-f0-9]{64}$");
    keys == (["schema","repository","source_sha","build_run_id","build_run_number","build_attempt","build_artifact_id","build_artifact_digest","version","team","execution_id","execution_attempt","automation_sha","checkpoint_id","checkpoint_digest","stage","phase","reason","submission_id","archive_sha256"]|sort) and
    .schema == 1 and all(to_entries[]; .key == "schema" or (.value|type) == "string") and
    .repository == $repo and .build_run_id == $build and .execution_id == $id and .execution_attempt == $attempt and .automation_sha == $sha and
    (.source_sha|test("^[a-f0-9]{40}$")) and
    ([.build_run_number,.build_attempt,.build_artifact_id,.checkpoint_id]|all(.[]; number)) and
    (.build_artifact_digest|digest) and (.checkpoint_digest|digest) and
    (.team|test("^[A-Z0-9]{10}$")) and (.version|test("^[A-Za-z0-9][A-Za-z0-9.+_-]*$")) and
    (.stage == "payload" or .stage == "installer" or .stage == "dmg") and
    (.phase == "submitted" or .phase == "accepted" or .phase == "finished" or .phase == "prepared" or .phase == "uploading") and
    ($mode != "signing" or .reason != "none") and
    (.reason == "none" or (.reason == "pending" and .phase == "submitted") or
      (.reason == "ticket-connection-loss" and (.phase == "accepted" or .phase == "finished"))) and
    (.archive_sha256|hash) and (if .phase == "prepared" or .phase == "uploading" then .submission_id == "" else
      (.submission_id|test("^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$")) end)
  ' "$temporary/recovery.json" >/dev/null || distribution_error 'Malformed or unbound recovery schema/context.'

  # A receipt must reference the newest checkpoint in its exact attempt/slot.
  jq -s -e --arg prefix "Apple-notarization-b$build-r$id-a" --argjson attempt "$attempt" '
    [.[]|select(.name|startswith("Apple-notarization-"))] |
    ([.[].name]|length) == ([.[].name]|unique|length) and all(.[];
      (.name|test("^" + $prefix + "[1-9][0-9]{0,14}-s(0[0-9]|10)$")) and
      (.name|capture("-a(?<attempt>[0-9]+)-s").attempt|tonumber) <= $attempt)' "$temporary/artifacts" >/dev/null ||
    distribution_error 'Malformed checkpoint slots.'
  checkpoint=$(jq -s -c '
    [.[]|select(.name|startswith("Apple-notarization-"))|
      . + (.name|capture("-a(?<attempt>[0-9]+)-s(?<slot>[0-9]+)$")) |
      .attempt |= tonumber | .slot |= tonumber] | sort_by(.attempt,.slot)|last // empty' "$temporary/artifacts")
  [[ -n $checkpoint ]] || distribution_error 'Referenced checkpoint is missing.'
  jq -e --slurpfile recovery "$temporary/recovery.json" --arg id "$id" --arg sha "$sha" --argjson slot "$slot" --argjson attempt "$receipt_attempt" '
    .expired == false and .slot == $slot and .attempt == $attempt and (.id|tostring) == $recovery[0].checkpoint_id and
    .digest == $recovery[0].checkpoint_digest and (.workflow_run.id|tostring) == $id and
    .workflow_run.head_branch == "main" and .workflow_run.head_sha == $sha
  ' <<< "$checkpoint" >/dev/null || distribution_error 'Newest checkpoint expired or does not bind the recovery receipt.'

  # Cross-check every retained diagnostic, not just the advertised reason.
  jq -e '
    def archive_names($stage; $r):
      (if $stage == "payload" then "Agent-Job-Queue-Payload-" else "Agent-Job-Queue-Installer-" end) as $prefix |
      (if $stage == "dmg" then "-universal.dmg" else "-universal.zip" end) as $suffix |
      [$prefix + $r.version + $suffix,
       $prefix + $r.version + "-build-" + $r.build_run_number + "-" + $r.source_sha[0:7] + $suffix];
    . as $e | ."recovery.json" as $r | ."metadata.json" as $m |
    $m.schema == 2 and $m.repository == $r.repository and $m.source_sha == $r.source_sha and
    $m.run_id == $r.build_run_id and $m.run_number == $r.build_run_number and $m.build_attempt == $r.build_attempt and
    $m.artifact_id == $r.build_artifact_id and $m.artifact_digest == $r.build_artifact_digest and
    $m.version == $r.version and $m.team == $r.team and ($m.initial_sha256|type == "string" and test("^[a-f0-9]{64}$")) and
    ($e["stage-" + $r.stage + ".json"] | .stage == $r.stage and .phase == $r.phase and
      .submission_id == $r.submission_id and .archive_sha256 == $r.archive_sha256) and
    all(["payload","installer","dmg"][]; . as $stage | $e["stage-" + $stage + ".json"] as $s |
      all(["log-","receipt-","status-"][]; . as $kind | $e[$kind + $stage + ".json"] as $d |
        $d == null or ($s != null and
          (if $kind == "status-" then $d.id == $s.submission_id and
            ($d.status == "Accepted" or ($stage == $r.stage and $r.reason == "pending" and $d.status == "In Progress"))
           else $d.jobId == $s.submission_id and $d.sha256 == $s.archive_sha256 and $d.status == "Accepted" and
             (if $d|has("archiveFilename") then $d.archiveFilename as $name | archive_names($stage; $r)|index($name) != null else true end) end)))) and
    (if $r.reason == "pending" then $e["status-" + $r.stage + ".json"] | .status == "In Progress" and .error == "timeout" and .execution_id == $r.execution_id and .execution_attempt == $r.execution_attempt
     elif $r.reason == "ticket-connection-loss" then
       $e["receipt-" + $r.stage + ".json"].status == "Accepted" and $e["log-" + $r.stage + ".json"].status == "Accepted"
     else true end)
  ' "$temporary/envelope.json" >/dev/null || distribution_error 'Retained evidence is missing, rejected or mismatched.'

  distribution_source_run "$build" "$temporary/source.json"
  jq -e --slurpfile recovery "$temporary/recovery.json" '
    .head_sha == $recovery[0].source_sha and (.run_number|tostring) == $recovery[0].build_run_number and
    .run_attempt >= ($recovery[0].build_attempt|tonumber)' "$temporary/source.json" >/dev/null || distribution_error 'Source build identity changed.'
  source_attempt=$(jq -r '.run_attempt' "$temporary/source.json")
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$build/artifacts" artifacts "$temporary/source-artifacts"
  jq -s -e --slurpfile recovery "$temporary/recovery.json" '
    $recovery[0] as $r | [.[]|select((.id|tostring) == $r.build_artifact_id)] | length == 1 and
    all(.[]; .name == ("Agent-Job-Queue-Installer-" + $r.build_attempt) and .expired == false and
      .digest == $r.build_artifact_digest and (.workflow_run.id|tostring) == $r.build_run_id and
      .workflow_run.head_branch == "main" and .workflow_run.head_sha == $r.source_sha)' "$temporary/source-artifacts" >/dev/null ||
    distribution_error 'Original build artifact missing, expired or mismatched.'
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$build/jobs?filter=latest" jobs "$temporary/source-jobs"
  jq -s -e --slurpfile recovery "$temporary/recovery.json" --argjson attempt "$source_attempt" '
    $recovery[0] as $r | all(.[]; (.run_id|tostring) == $r.build_run_id and .head_sha == $r.source_sha and
      (.run_attempt|type) == "number" and .run_attempt >= 1 and .run_attempt <= $attempt and .run_attempt == (.run_attempt|floor)) and
    ([.[]|select(.name == "ci-required")]|length) == 1 and
    all(.[]|select(.name == "ci-required"); .status == "completed" and .conclusion == "success")' "$temporary/source-jobs" >/dev/null ||
    distribution_error 'Source effective ci-required gate has not succeeded.'
  artifact=$(jq -r '.build_attempt' "$temporary/recovery.json")
  distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$build/attempts/$artifact/jobs" jobs "$temporary/build-jobs"
  jq -s -e --slurpfile recovery "$temporary/recovery.json" '
    $recovery[0] as $r | all(.[]; (.run_id|tostring) == $r.build_run_id and .head_sha == $r.source_sha and
      (.run_attempt|tostring) == $r.build_attempt) and
    ([.[]|select(.name == "Universal installer")]|length) == 1 and
    all(.[]|select(.name == "Universal installer"); .status == "completed" and .conclusion == "success")' "$temporary/build-jobs" >/dev/null ||
    distribution_error 'Original installer attempt did not succeed.'

  # Re-observe the group immediately before mutation; a new active or successful
  # execution, or a changed attempt, invalidates the decision.
  recovery_runs "$temporary/fresh-runs"
  jq -e --arg title "$title" --argjson expected "$candidate" '
    [.[]|select(.display_title == $title)] as $group |
    all($group[]; .status == "completed" and .conclusion != "success") and
    ([$group[]|select(.conclusion == "failure")]|max_by(.id)) == $expected' "$temporary/fresh-runs" >/dev/null ||
    distribution_error 'Candidate group changed, is active or has succeeded.'
  # Count eligible requests before POST, including provider refusals, so an API
  # outage cannot turn the five-candidate budget into an unbounded write loop.
  printf '%s\n' "$id" >> "$temporary/eligible" || distribution_error 'Cannot record the recovery request budget.'
  gh api --method POST "repos/$GITHUB_REPOSITORY/actions/runs/$id/rerun-failed-jobs" >/dev/null || distribution_error 'GitHub refused failed-job recovery.'
  printf 'Recovery requested: build=%s execution=%s next_attempt=%s\n' "$build" "$id" "$((attempt + 1))"
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    printf 'Requested same-run continuation: build %s, [execution %s](https://github.com/%s/actions/runs/%s), attempt %s.\n\n' \
      "$build" "$id" "$GITHUB_REPOSITORY" "$id" "$((attempt + 1))" >> "$GITHUB_STEP_SUMMARY"
  fi
)

recovery_runs "$temporary/runs"
jq -c 'group_by(.display_title)[] |
  select(all(.[]; .status == "completed" and .conclusion != "success")) |
  [.[]|select(.conclusion == "failure")] | max_by(.id) // empty' "$temporary/runs" > "$temporary/candidates"
recovered=0
: > "$temporary/eligible"
while IFS= read -r candidate; do
  if recovery_candidate "$candidate"; then
    recovered=$((recovered + 1))
  else
    printf 'Recovery skipped: execution=%s; inspect the reason above.\n' "$(jq -r '.id' <<< "$candidate")" >&2
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      printf 'Continuation held: [execution %s](https://github.com/%s/actions/runs/%s). Inspect the exact refusal in this observer log; no replacement upload was authorized.\n\n' \
        "$(jq -r '.id' <<< "$candidate")" "$GITHUB_REPOSITORY" "$(jq -r '.id' <<< "$candidate")" >> "$GITHUB_STEP_SUMMARY"
    fi
  fi
  (( $(wc -l < "$temporary/eligible") < 5 )) || break
done < "$temporary/candidates"
printf 'Recovery observer finished: %s same-run retries requested.\n' "$recovered"
