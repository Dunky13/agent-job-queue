#!/bin/bash
# Persist only the public resumable notarization state, never signing credentials.
set -euo pipefail
umask 077

checkpoint_error() {
  printf 'macOS checkpoint: %s\n' "$*" >&2
  exit 1
}

checkpoint_context() {
  [[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF:-} == refs/heads/main ]] ||
    checkpoint_error 'Recovery requires a trusted push to main.'
  local sha_pattern='^[a-fA-F0-9]{40}$' number_pattern='^[1-9][0-9]{0,14}$'
  local repository_pattern='^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' name
  [[ ${GITHUB_SHA:-} =~ $sha_pattern && ${GITHUB_REPOSITORY:-} =~ $repository_pattern ]] ||
    checkpoint_error 'Invalid source identity.'
  for name in GITHUB_RUN_ID GITHUB_RUN_NUMBER GITHUB_RUN_ATTEMPT; do
    [[ ${!name:-} =~ $number_pattern ]] || checkpoint_error "Invalid $name."
  done
}

checkpoint_output() {
  [[ -n ${GITHUB_OUTPUT:-} ]] || checkpoint_error 'GITHUB_OUTPUT is required.'
  printf 'restored=%s\n' "$1" >> "$GITHUB_OUTPUT"
}

# Check ZIP paths before extraction. Ditto carries resource forks in __MACOSX.
# Signed bundles remain inside immutable ZIP files, so the state envelope needs
# no symlinks. No archive content is ever executed.
checkpoint_archive() {
  local archive=$1 kind=$2 listing=$3 entry count modes path_pattern='^[A-Za-z0-9./_+() -]+$'
  unzip -Z1 "$archive" > "$listing.names" || checkpoint_error 'Unreadable checkpoint ZIP.'
  unzip -Z -l "$archive" > "$listing.modes" || checkpoint_error 'Unreadable checkpoint ZIP metadata.'
  [[ -s $listing.names ]] || checkpoint_error 'Empty checkpoint ZIP.'
  LC_ALL=C grep -q '[[:cntrl:]]' "$listing.names" && checkpoint_error 'Control character in archive path.'
  count=$(wc -l < "$listing.names" | tr -d ' ')
  modes=$(awk 'length($1) == 10 && $1 ~ /^[-dl]/ {n++} END {print n+0}' "$listing.modes")
  [[ $count == "$modes" ]] || checkpoint_error 'Unsupported archive entry or ambiguous filename.'
  ! grep -q '^l' "$listing.modes" || checkpoint_error 'Checkpoint archive contains a symlink.'
  [[ $(LC_ALL=C sort "$listing.names" | uniq -d | wc -l | tr -d ' ') == 0 ]] || checkpoint_error 'Duplicate archive path.'
  while IFS= read -r entry; do
    [[ $entry =~ $path_pattern ]] || checkpoint_error 'Unsafe archive path character.'
    case "$entry" in
      /*|*//*|../*|*/../*|*/..|./*|*/./*|*/.) checkpoint_error 'Unsafe archive path.' ;;
    esac
    if [[ $kind == outer ]]; then
      case "$entry" in checkpoint.zip|checkpoint.zip.sha256) ;; *) checkpoint_error 'Unexpected outer archive entry.' ;; esac
    else
      case "$entry" in notarization-state/.lock|notarization-state/.lock/*|notarization-state/work|notarization-state/work/*)
        checkpoint_error 'Checkpoint contains active or temporary work.' ;; esac
      case "$entry" in notarization-state/|notarization-state/*|__MACOSX/|__MACOSX/notarization-state/|__MACOSX/notarization-state/*|__MACOSX/._notarization-state) ;;
        *) checkpoint_error 'Unexpected state archive root.' ;; esac
    fi
  done < "$listing.names"
  if [[ $kind == outer ]]; then
    [[ $count == 2 ]] || checkpoint_error 'Incomplete outer archive.'
    return
  fi
}

checkpoint_snapshot() {
  local state=${1%/} output=${2%/} temporary mode
  [[ ${state##*/} == notarization-state && -d $state && ! -L $state ]] || checkpoint_error 'A real notarization-state directory is required.'
  state=$(cd "$state" && pwd -P)
  mode=$(stat -f '%Lp' "$state" 2>/dev/null) || mode=$(stat -c '%a' "$state")
  [[ $mode == 700 ]] || checkpoint_error 'The state directory must have private mode 700.'
  [[ ! -e $state/.lock && ! -L $state/.lock && ! -e $state/work && ! -L $state/work ]] ||
    checkpoint_error 'Cannot snapshot active or temporary work.'
  [[ ! -L $output ]] || checkpoint_error 'Snapshot output cannot be a symlink.'
  mkdir -p "$output"
  output=$(cd "$output" && pwd -P)
  case "$output/" in "$state/"*) checkpoint_error 'Snapshot output must be outside the state directory.' ;; esac
  [[ ! -e $output/checkpoint.zip && ! -L $output/checkpoint.zip && ! -e $output/checkpoint.zip.sha256 && ! -L $output/checkpoint.zip.sha256 ]] ||
    checkpoint_error 'Refusing to replace a checkpoint.'
  temporary=$(mktemp -d)
  CHECKPOINT_TEMPORARY=$temporary
  trap 'rm -rf "$CHECKPOINT_TEMPORARY"' EXIT
  # Share the engine's lock so a snapshot cannot race a state transition.
  mkdir "$state/.lock" 2>/dev/null || checkpoint_error 'State is locked by another command.'
  CHECKPOINT_LOCK="$state/.lock"
  trap 'rmdir "$CHECKPOINT_LOCK"; rm -rf "$CHECKPOINT_TEMPORARY"' EXIT
  ditto "$state" "$temporary/notarization-state"
  rmdir "$temporary/notarization-state/.lock"
  ditto -c -k --sequesterRsrc --keepParent "$temporary/notarization-state" "$temporary/checkpoint.zip"
  checkpoint_archive "$temporary/checkpoint.zip" inner "$temporary/inner"
  (cd "$temporary" && shasum -a 256 checkpoint.zip > checkpoint.zip.sha256)
  mv "$temporary/checkpoint.zip" "$temporary/checkpoint.zip.sha256" "$output/"
}

checkpoint_restore() {
  local state=${1%/} temporary metadata page=1 collected=0 total=-1 page_count page_total selected artifact_id digest actual expected parent
  [[ ${state##*/} == notarization-state && ! -e $state && ! -L $state ]] || checkpoint_error 'Restore requires an absent notarization-state directory.'
  parent=$(dirname "$state")
  [[ -d $parent ]] || checkpoint_error 'Restore parent directory must already exist.'
  state="$(cd "$parent" && pwd -P)/notarization-state"
  temporary=$(mktemp -d "${state%/*}/.checkpoint.XXXXXX")
  CHECKPOINT_TEMPORARY=$temporary
  trap 'rm -rf "$CHECKPOINT_TEMPORARY"' EXIT
  metadata="$temporary/run.json"
  gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID" > "$metadata"
  jq -e --arg repository "$GITHUB_REPOSITORY" --arg sha "$GITHUB_SHA" --arg id "$GITHUB_RUN_ID" --arg number "$GITHUB_RUN_NUMBER" '
    (.id|tostring) == $id and (.run_number|tostring) == $number and
    .path == ".github/workflows/ci.yml" and .event == "push" and .head_branch == "main" and .head_sha == $sha and
    .repository.full_name == $repository and .head_repository.full_name == $repository
  ' "$metadata" >/dev/null || checkpoint_error 'Run provenance does not match this trusted main push.'
  : > "$temporary/artifacts.jsonl"
  while :; do
    gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100&page=$page" > "$temporary/page.json"
    jq -e '(.total_count|type) == "number" and .total_count >= 0 and .total_count == (.total_count|floor) and
      (.artifacts|type) == "array" and (.artifacts|length) <= 100' "$temporary/page.json" >/dev/null || checkpoint_error 'Malformed artifact page.'
    page_total=$(jq -r '.total_count' "$temporary/page.json")
    page_count=$(jq -r '.artifacts|length' "$temporary/page.json")
    if [[ $total == -1 ]]; then total=$page_total; fi
    [[ $page_total == "$total" ]] || checkpoint_error 'Artifact inventory changed during recovery.'
    jq -c '.artifacts[]' "$temporary/page.json" >> "$temporary/artifacts.jsonl"
    collected=$((collected + page_count))
    ((collected <= total)) || checkpoint_error 'Artifact count exceeds inventory.'
    ((collected < total)) || break
    ((page_count > 0)) || checkpoint_error 'Truncated artifact inventory.'
    page=$((page + 1))
  done
  jq -s -e 'all(.[]; (.id|type) == "number" and .id > 0 and .id == (.id|floor) and (.name|type) == "string") and
    ([.[].id]|length) == ([.[].id]|unique|length)' "$temporary/artifacts.jsonl" >/dev/null || checkpoint_error 'Malformed or duplicate artifact identity.'
  jq -s -e --arg run "$GITHUB_RUN_ID" --arg sha "$GITHUB_SHA" --argjson attempt "$GITHUB_RUN_ATTEMPT" '
    [.[] | select(.name|startswith("Apple-notarization-"))] |
    ([.[].name]|length) == ([.[].name]|unique|length) and all(.[];
      (.name|test("^Apple-notarization-" + $run + "-a[1-9][0-9]{0,14}-s0[1-9]$")) and
      (.name|capture("-a(?<attempt>[0-9]+)-s").attempt|tonumber) <= $attempt and
      (.workflow_run.id|tostring) == $run and .workflow_run.head_branch == "main" and .workflow_run.head_sha == $sha)
  ' "$temporary/artifacts.jsonl" >/dev/null || checkpoint_error 'Mixed or malformed checkpoint provenance.'
  selected=$(jq -s -c --argjson attempt "$GITHUB_RUN_ATTEMPT" '
    [.[] | select(.name|startswith("Apple-notarization-")) |
      . + (.name|capture("-a(?<attempt>[0-9]+)-s(?<slot>[0-9]+)$"))|.attempt |= tonumber|.slot |= tonumber|
      select(.attempt < $attempt)] | sort_by(.attempt,.slot) | last // empty
  ' "$temporary/artifacts.jsonl")
  if [[ -z $selected ]]; then
    [[ $GITHUB_RUN_ATTEMPT == 1 ]] || checkpoint_error 'Rerun has no surviving checkpoint; refusing a fresh submission.'
    checkpoint_output false
    return
  fi
  [[ $(jq -r '.expired' <<< "$selected") == false ]] || checkpoint_error 'Newest checkpoint expired; refusing an older state.'
  artifact_id=$(jq -r '.id' <<< "$selected")
  digest=$(jq -r '.digest' <<< "$selected")
  [[ $digest =~ ^sha256:[a-f0-9]{64}$ ]] || checkpoint_error 'Checkpoint artifact has no valid GitHub SHA-256 digest.'
  gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact_id/zip" > "$temporary/artifact.zip"
  actual=$(shasum -a 256 "$temporary/artifact.zip"); actual=${actual%% *}
  [[ $digest == "sha256:$actual" ]] || checkpoint_error 'GitHub artifact digest mismatch.'
  checkpoint_archive "$temporary/artifact.zip" outer "$temporary/outer"
  mkdir "$temporary/outer"
  ditto -x -k "$temporary/artifact.zip" "$temporary/outer"
  expected=$(cat "$temporary/outer/checkpoint.zip.sha256")
  [[ $expected =~ ^[a-f0-9]{64}'  checkpoint.zip'$ ]] || checkpoint_error 'Invalid inner checkpoint digest.'
  actual=$(shasum -a 256 "$temporary/outer/checkpoint.zip"); actual=${actual%% *}
  [[ ${expected%% *} == "$actual" ]] || checkpoint_error 'Inner checkpoint digest mismatch.'
  checkpoint_archive "$temporary/outer/checkpoint.zip" inner "$temporary/inner"
  mkdir "$temporary/state"
  ditto -x -k "$temporary/outer/checkpoint.zip" "$temporary/state"
  [[ -d $temporary/state/notarization-state && ! -L $temporary/state/notarization-state ]] || checkpoint_error 'Missing restored state directory.'
  [[ ! -e $state && ! -L $state ]] || checkpoint_error 'Restore destination appeared during recovery.'
  mv "$temporary/state/notarization-state" "$state"
  checkpoint_output true
}

checkpoint_context
case "${1:-}" in
  snapshot) [[ $# == 3 ]] || checkpoint_error 'Usage: macos-checkpoint.sh snapshot STATE_DIR OUT_DIR'; checkpoint_snapshot "$2" "$3" ;;
  restore) [[ $# == 2 ]] || checkpoint_error 'Usage: macos-checkpoint.sh restore STATE_DIR'; checkpoint_restore "$2" ;;
  *) checkpoint_error 'Usage: macos-checkpoint.sh {snapshot STATE_DIR OUT_DIR|restore STATE_DIR}' ;;
esac
