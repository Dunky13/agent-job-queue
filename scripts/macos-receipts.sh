#!/bin/bash
# Small public recovery evidence, independent of the large immutable archives.
set -euo pipefail
umask 077
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/distribution-common.sh
source "$root/scripts/distribution-common.sh"
distribution_context
[[ $# == 5 ]] || distribution_error 'Usage: macos-receipts.sh STATE OUTPUT STAGE CHECKPOINT_ID CHECKPOINT_DIGEST'
state=$1 output=$2 stage=$3 checkpoint_id=$4 checkpoint_digest="sha256:${5#sha256:}"
case "$stage" in payload|installer|dmg) ;; *) distribution_error 'Invalid receipt stage.' ;; esac
distribution_number "$checkpoint_id" || distribution_error 'A persisted checkpoint ID is required.'
[[ $checkpoint_digest =~ ^sha256:[a-f0-9]{64}$ ]] || distribution_error 'A persisted checkpoint digest is required.'
[[ ! -e $output && ! -L $output && -d $state && ! -L $state ]] || distribution_error 'Fresh receipt output and real state are required.'
"$root/scripts/macos-resume.sh" verify-state "$state" "$BUILD_VERSION"
mkdir -m 700 "$output"
cp "$state/metadata.json" "$output/metadata.json"
# Only explicit public files enter the receipt envelope. Private command output,
# work directories, credentials and arbitrary state files are never copied.
for name in payload installer dmg; do
  for pair in "stages/$name.json:stage-$name.json" "receipts/$name.json:receipt-$name.json" \
    "receipts/$name.log.json:log-$name.json" "receipts/$name.status.json:status-$name.json"; do
    source_file="$state/${pair%%:*}"
    if [[ -e $source_file ]]; then
      [[ -f $source_file && ! -L $source_file ]] || distribution_error 'Unsafe public receipt file.'
      cp "$source_file" "$output/${pair#*:}"
    fi
  done
done
if [[ -f ${APPLE_TOOLCHAIN_RECEIPT:-} && ! -L ${APPLE_TOOLCHAIN_RECEIPT:-} ]]; then
  cp "$APPLE_TOOLCHAIN_RECEIPT" "$output/toolchain.json"
fi
record="$state/stages/$stage.json"
phase=prepared submission='' hash='' reason=none
if [[ -f $record ]]; then
  phase=$(jq -er '.phase' "$record")
  submission=$(jq -r '.submission_id' "$record")
  hash=$(jq -er '.archive_sha256' "$record")
  if [[ $phase == submitted && -f $state/receipts/$stage.status.json ]] &&
    jq -e --arg id "$submission" --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" '
      .id == $id and .status == "In Progress" and .error == "timeout" and
      .execution_id == $run and .execution_attempt == $attempt' \
      "$state/receipts/$stage.status.json" >/dev/null; then
    reason=pending
  elif [[ ( $phase == accepted || $phase == finished ) && -f $state/receipts/$stage.retry && ! -L $state/receipts/$stage.retry &&
    $(cat "$state/receipts/$stage.retry") == ticket-connection-loss ]]; then
    reason=ticket-connection-loss
  fi
fi
# Only the outer credential wrapper can establish successful cleanup. Its
# current-invocation outcome lives outside the retained state and is never
# restored from another runner. A preparation/setup failure has no such proof.
if [[ $reason != none ]]; then
  if [[ ! -f ${APPLE_SIGNING_RESULT_FILE:-} || -L ${APPLE_SIGNING_RESULT_FILE:-} ]] || ! jq -e \
    --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" --arg operation "$stage-finish" '
      .cleanup == "success" and .execution_id == $run and .execution_attempt == $attempt and .operation == $operation
    ' "$APPLE_SIGNING_RESULT_FILE" >/dev/null; then reason=none; fi
fi
# A rejection or log/UUID/hash disagreement always removes retry authority.
if [[ $reason != none && -f $state/receipts/$stage.log.json ]] && ! jq -e --arg id "$submission" --arg hash "$hash" '
    .jobId == $id and .sha256 == $hash and (.status == "Accepted" or .status == "In Progress")
  ' "$state/receipts/$stage.log.json" >/dev/null; then reason=none; fi
jq -n --arg repository "$GITHUB_REPOSITORY" --arg sha "$BUILD_SHA" \
  --arg run "$BUILD_RUN_ID" --arg number "$BUILD_RUN_NUMBER" --arg attempt "$BUILD_RUN_ATTEMPT" \
  --arg artifact "$BUILD_ARTIFACT_ID" --arg digest "$BUILD_ARTIFACT_DIGEST" \
  --arg version "$BUILD_VERSION" --arg team "$APPLE_TEAM_ID" \
  --arg execution "$GITHUB_RUN_ID" --arg execution_attempt "$GITHUB_RUN_ATTEMPT" --arg automation "$GITHUB_SHA" \
  --arg checkpoint "$checkpoint_id" --arg checkpoint_digest "$checkpoint_digest" \
  --arg stage "$stage" --arg phase "$phase" --arg reason "$reason" --arg id "$submission" --arg hash "$hash" '
  {schema:1,repository:$repository,source_sha:$sha,build_run_id:$run,build_run_number:$number,
   build_attempt:$attempt,build_artifact_id:$artifact,build_artifact_digest:$digest,version:$version,team:$team,
   execution_id:$execution,execution_attempt:$execution_attempt,automation_sha:$automation,
   checkpoint_id:$checkpoint,checkpoint_digest:$checkpoint_digest,stage:$stage,phase:$phase,reason:$reason,
   submission_id:$id,archive_sha256:$hash}
' > "$output/recovery.json"
printf 'Public receipt: stage=%s phase=%s recovery=%s submission=%s\n' "$stage" "$phase" "$reason" "$submission"
