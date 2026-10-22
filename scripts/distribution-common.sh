#!/bin/bash
# Shared fail-closed GitHub provenance and paginated inventory checks.
distribution_error() { printf 'distribution: %s\n' "$*" >&2; exit 1; }

distribution_number() { [[ $1 =~ ^[1-9][0-9]{0,14}$ ]]; }

distribution_context() {
  [[ ${GITHUB_ACTIONS:-} == true && ${GITHUB_REF:-} == refs/heads/main &&
    ${GITHUB_REPOSITORY:-} =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ &&
    ${GITHUB_SHA:-} =~ ^[a-f0-9]{40}$ ]] || distribution_error 'Trusted main automation is required.'
  case "${GITHUB_EVENT_NAME:-}" in workflow_run|workflow_dispatch) ;; *) distribution_error 'Unexpected distribution event.' ;; esac
  local name
  for name in GITHUB_RUN_ID GITHUB_RUN_NUMBER GITHUB_RUN_ATTEMPT; do
    distribution_number "${!name:-}" || distribution_error "Invalid $name."
  done
}

# Callers keep inventories in private temporary directories. Never truncate at
# the API's first page or accept a changing/duplicated inventory.
distribution_inventory() {
  local endpoint=$1 key=$2 destination=$3 page=1 total=-1 collected=0 count observed separator='?'
  [[ $endpoint != *'?'* ]] || separator='&'
  : > "$destination"
  while :; do
    gh api "${endpoint}${separator}per_page=100&page=$page" > "$destination.page"
    jq -e --arg key "$key" '(.total_count|type) == "number" and .total_count >= 0 and
      .total_count == (.total_count|floor) and (.[$key]|type) == "array" and (.[$key]|length) <= 100' \
      "$destination.page" >/dev/null || distribution_error 'Malformed inventory page.'
    observed=$(jq -r '.total_count' "$destination.page")
    count=$(jq -r --arg key "$key" '.[$key]|length' "$destination.page")
    if [[ $total == -1 ]]; then total=$observed; fi
    [[ $total == "$observed" ]] || distribution_error 'Inventory changed during recovery.'
    jq -c --arg key "$key" '.[$key][]' "$destination.page" >> "$destination"
    collected=$((collected + count))
    ((collected <= total)) || distribution_error 'Inventory exceeds declared count.'
    ((collected < total)) || break
    ((count > 0 && page < 10000)) || distribution_error 'Truncated inventory.'
    page=$((page + 1))
  done
  jq -s -e 'all(.[]; (.id|type) == "number" and .id > 0 and .id < 1e15 and .id == (.id|floor)) and
    ([.[].id]|length) == ([.[].id]|unique|length)' "$destination" >/dev/null || distribution_error 'Malformed or duplicate inventory identity.'
}

distribution_source_run() {
  gh api "repos/$GITHUB_REPOSITORY/actions/runs/$1" > "$2"
  jq -e --arg repository "$GITHUB_REPOSITORY" --arg id "$1" '
    (.id|tostring) == $id and .path == ".github/workflows/ci.yml" and .event == "push" and
    .head_branch == "main" and (.head_sha|test("^[a-f0-9]{40}$")) and
    (.run_number|tostring|test("^[1-9][0-9]{0,8}$")) and
    (.run_attempt|tostring|test("^[1-9][0-9]{0,14}$")) and .status == "completed" and
    .repository.full_name == $repository and .head_repository.full_name == $repository
  ' "$2" >/dev/null || distribution_error 'Source run provenance does not match a completed same-repository main-push CI build.'
}
