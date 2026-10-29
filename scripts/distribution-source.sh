#!/bin/bash
# Select original CI bytes without checking out or executing source-build code.
set -euo pipefail
umask 077
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
# shellcheck source=scripts/distribution-common.sh
source "$root/scripts/distribution-common.sh"
distribution_context
[[ $# == 2 && ( $1 == select || $1 == download ) ]] || distribution_error 'Usage: distribution-source.sh {select|download} OUTPUT_DIR'
command=$1 destination=$2
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
if [[ $command == select ]]; then
  case "$GITHUB_EVENT_NAME" in
    workflow_dispatch)
      source_id=${INPUT_BUILD_RUN_ID:-}
      distribution_number "$source_id" || distribution_error 'build_run_id must be a positive numeric CI run ID.'
      ;;
    workflow_run)
      [[ -f ${GITHUB_EVENT_PATH:-} ]] || distribution_error 'workflow_run event is missing.'
      jq -e --arg repo "$GITHUB_REPOSITORY" '
        .action == "completed" and .workflow_run.conclusion == "success" and .workflow_run.event == "push" and
        .workflow_run.path == ".github/workflows/ci.yml" and .workflow_run.head_branch == "main" and
        .repository.full_name == $repo and .workflow_run.repository.full_name == $repo and
        .workflow_run.head_repository.full_name == $repo
      ' "$GITHUB_EVENT_PATH" >/dev/null || distribution_error 'Automatic source must be a successful same-repository main-push CI run.'
      source_id=$(jq -r '.workflow_run.id' "$GITHUB_EVENT_PATH")
      distribution_number "$source_id" || distribution_error 'Invalid automatic source ID.'
      ;;
  esac
else
  source_id=${BUILD_RUN_ID:-}
  distribution_number "$source_id" || distribution_error 'Verified BUILD_RUN_ID is required.'
fi
distribution_source_run "$source_id" "$temporary/run.json"
if [[ $command == select && $GITHUB_EVENT_NAME == workflow_run ]]; then
  jq -e --slurpfile event "$GITHUB_EVENT_PATH" '
    .conclusion == "success" and .head_sha == $event[0].workflow_run.head_sha and
    .run_attempt == $event[0].workflow_run.run_attempt and .run_number == $event[0].workflow_run.run_number
  ' "$temporary/run.json" >/dev/null || distribution_error 'Automatic source changed since the successful CI event.'
fi
sha=$(jq -r '.head_sha' "$temporary/run.json")
number=$(jq -r '.run_number' "$temporary/run.json")
attempt=$(jq -r '.run_attempt' "$temporary/run.json")
distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$source_id/artifacts" artifacts "$temporary/artifacts"
jq -s -e --arg id "$source_id" --arg sha "$sha" --argjson attempt "$attempt" '
  [.[] | select(.name|startswith("Agent-Queue-App-"))] |
  ([.[].name]|length) == ([.[].name]|unique|length) and all(.[];
    (.name|test("^Agent-Queue-App-[1-9][0-9]{0,14}$")) and
    (.name|split("-")|last|tonumber) <= $attempt and
    (.workflow_run.id|tostring) == $id and .workflow_run.head_branch == "main" and .workflow_run.head_sha == $sha)
' "$temporary/artifacts" >/dev/null || distribution_error 'Mixed or malformed source artifact provenance.'
selected=$(jq -s -c '[.[] | select(.name|startswith("Agent-Queue-App-")) |
  . + {attempt:(.name|split("-")|last|tonumber)}] | sort_by(.attempt) | last // empty' "$temporary/artifacts")
[[ -n $selected ]] || distribution_error 'Source app artifact is missing.'
[[ $(jq -r '.expired' <<< "$selected") == false ]] || distribution_error 'Newest source app artifact expired.'
artifact_id=$(jq -r '.id' <<< "$selected")
artifact_digest=$(jq -r '.digest' <<< "$selected")
artifact_attempt=$(jq -r '.attempt' <<< "$selected")
[[ $artifact_digest =~ ^sha256:[a-f0-9]{64}$ ]] || distribution_error 'Source artifact requires a GitHub SHA-256 digest.'
# The retained app must have passed in its producing attempt. Failed-job
# retries can repair other validations without rebuilding those original bytes.
distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$source_id/attempts/$artifact_attempt/jobs" jobs "$temporary/jobs"
jq -s -e --arg id "$source_id" --arg sha "$sha" --argjson attempt "$artifact_attempt" '
  all(.[]; (.run_id|tostring) == $id and .head_sha == $sha and .run_attempt == $attempt) and
  ([.[] | select(.name == "Universal app")]|length) == 1 and
  all(.[] | select(.name == "Universal app"); .conclusion == "success")
' "$temporary/jobs" >/dev/null || distribution_error 'Source app attempt did not pass app validation.'
# filter=latest returns each job effective for this run, including successful
# jobs retained from earlier attempts. It does not mean the artifact's attempt.
distribution_inventory "repos/$GITHUB_REPOSITORY/actions/runs/$source_id/jobs?filter=latest" jobs "$temporary/effective-jobs"
jq -s -e --arg id "$source_id" --arg sha "$sha" --argjson attempt "$attempt" '
  all(.[]; (.run_id|tostring) == $id and .head_sha == $sha and
    (.run_attempt|type) == "number" and .run_attempt >= 1 and .run_attempt <= $attempt and
    .run_attempt == (.run_attempt|floor)) and
  ([.[] | select(.name == "ci-required")]|length) == 1 and
  all(.[] | select(.name == "ci-required"); .status == "completed" and .conclusion == "success")
' "$temporary/effective-jobs" >/dev/null || distribution_error 'Source run did not pass effective required validation gates.'
if [[ $command == download ]]; then
  [[ $BUILD_SHA == "$sha" && $BUILD_RUN_NUMBER == "$number" && $BUILD_RUN_ATTEMPT == "$artifact_attempt" &&
    $BUILD_ARTIFACT_ID == "$artifact_id" && $BUILD_ARTIFACT_DIGEST == "$artifact_digest" ]] ||
    distribution_error 'Selected source artifact changed; refusing different build bytes.'
fi
gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact_id/zip" > "$temporary/artifact.zip"
version=$("${QUEUE_NOTARY_TOOL:?Prebuilt trusted queue-tool is required}" verify-build-artifact \
  --archive "$temporary/artifact.zip" --digest "$artifact_digest" --commit "$sha" --build "$number" --output "$destination")
if [[ $command == download ]]; then
  [[ $BUILD_VERSION == "$version" && $BUILD_TAG == "build-$number-${sha:0:7}" ]] || distribution_error 'Build version or tag changed.'
else
  [[ -n ${GITHUB_OUTPUT:-} ]] || distribution_error 'GITHUB_OUTPUT is required.'
  {
    printf 'run_id=%s\nrun_number=%s\nrun_attempt=%s\nsha=%s\n' "$source_id" "$number" "$artifact_attempt" "$sha"
    printf 'artifact_id=%s\nartifact_digest=%s\nversion=%s\ntag=build-%s-%.7s\n' "$artifact_id" "$artifact_digest" "$version" "$number" "$sha"
    printf 'automation_sha=%s\n' "$(git -C "$root" rev-parse HEAD)"
  } >> "$GITHUB_OUTPUT"
fi
