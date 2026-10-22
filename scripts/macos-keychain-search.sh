#!/bin/bash
# Register only an owned disposable keychain on a disposable GitHub-hosted job.
# This module does not change the default keychain or certificate trust.
macos_keychain_search_work=
macos_keychain_search_restore_needed=0
macos_keychain_search_original_count=0
macos_keychain_search_original=()
macos_keychain_search_expected=()

macos_keychain_search_diagnostic() {
  printf 'Apple signing keychain search: phase=%s classification=%s.\n' "$1" "$2" >&2
}

macos_keychain_search_read() {
  local phase=$1 line path previous status
  local row_pattern='^[ ]*"(/[^"\\]*)"[ ]*$'
  macos_keychain_search_read_count=0
  macos_keychain_search_read_paths=()
  if ! LC_ALL=C security list-keychains -d user > "$macos_keychain_search_work/current" 2>/dev/null; then
    macos_keychain_search_diagnostic "$phase" query-failed
    return 1
  fi
  # Reject control bytes before shell parsing, including NUL, which Bash cannot
  # represent. Newline record separators are not part of grep's line matches.
  if LC_ALL=C grep -q '[[:cntrl:]]' "$macos_keychain_search_work/current"; then
    macos_keychain_search_diagnostic "$phase" malformed
    return 1
  else
    status=$?
    if [[ $status != 1 ]]; then
      macos_keychain_search_diagnostic "$phase" query-failed
      return 1
    fi
  fi
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ ! $line =~ $row_pattern ]]; then
      macos_keychain_search_diagnostic "$phase" malformed
      return 1
    fi
    path=${BASH_REMATCH[1]}
    previous=0
    while ((previous < macos_keychain_search_read_count)); do
      if [[ ${macos_keychain_search_read_paths[$previous]} == "$path" ]]; then
        macos_keychain_search_diagnostic "$phase" malformed
        return 1
      fi
      previous=$((previous + 1))
    done
    macos_keychain_search_read_paths[macos_keychain_search_read_count]=$path
    macos_keychain_search_read_count=$((macos_keychain_search_read_count + 1))
  done < "$macos_keychain_search_work/current"
}

macos_keychain_search_matches() {
  local kind=$1 count=$2 index=0
  [[ $macos_keychain_search_read_count == "$count" ]] || return 1
  while ((index < count)); do
    if [[ $kind == original ]]; then
      [[ ${macos_keychain_search_read_paths[$index]} == "${macos_keychain_search_original[$index]}" ]] || return 1
    else
      [[ ${macos_keychain_search_read_paths[$index]} == "${macos_keychain_search_expected[$index]}" ]] || return 1
    fi
    index=$((index + 1))
  done
}

macos_keychain_search_register() {
  # Check both guards before reading any keychain settings or creating state.
  if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted ]]; then
    macos_keychain_search_diagnostic register forbidden-runner
    return 1
  fi
  if [[ $# != 1 || $1 != /* || ! -f $1 || $1 == *'"'* || $1 == *\\* || $1 =~ [[:cntrl:]] ||
    -n $macos_keychain_search_work ]]; then
    macos_keychain_search_diagnostic register invalid-owned-keychain
    return 1
  fi
  macos_keychain_search_original=()
  macos_keychain_search_expected=()
  macos_keychain_search_work=$(umask 077; mktemp -d "${TMPDIR:-/tmp}/agent-keychain-search.XXXXXX") || {
    macos_keychain_search_diagnostic register private-state-failed
    return 1
  }
  macos_keychain_search_read register || return
  macos_keychain_search_original_count=$macos_keychain_search_read_count
  if ((macos_keychain_search_original_count > 0)); then
    macos_keychain_search_original=("${macos_keychain_search_read_paths[@]}")
    macos_keychain_search_expected=("${macos_keychain_search_read_paths[@]}")
  fi
  local index=0
  while ((index < macos_keychain_search_original_count)); do
    if [[ ${macos_keychain_search_original[$index]} == "$1" ]]; then
      macos_keychain_search_diagnostic register already-present
      return 1
    fi
    index=$((index + 1))
  done
  macos_keychain_search_expected[macos_keychain_search_original_count]=$1
  # A setter can mutate state and still fail, so cleanup owns restoration before
  # the first write. No credential values or search-list paths are logged.
  macos_keychain_search_restore_needed=1
  if ! security list-keychains -d user -s "${macos_keychain_search_expected[@]}" >/dev/null 2>&1; then
    macos_keychain_search_diagnostic register set-failed
    return 1
  fi
  macos_keychain_search_read register || return
  if ! macos_keychain_search_matches expected "$((macos_keychain_search_original_count + 1))"; then
    macos_keychain_search_diagnostic register readback-mismatch
    return 1
  fi
  macos_keychain_search_diagnostic register ready
}

macos_keychain_search_restore_list() {
  macos_keychain_search_read restore || return
  # A failed setter may have left the original list unchanged.
  if macos_keychain_search_matches original "$macos_keychain_search_original_count"; then
    macos_keychain_search_diagnostic restore ready
    return 0
  fi
  # Preserve unexpected concurrent entries. The caller still deletes its owned
  # keychain, but a conflicting list must never be replaced with our snapshot.
  if ! macos_keychain_search_matches expected "$((macos_keychain_search_original_count + 1))"; then
    macos_keychain_search_diagnostic restore conflict
    return 1
  fi
  if ((macos_keychain_search_original_count == 0)); then
    security list-keychains -d user -s >/dev/null 2>&1 || {
      macos_keychain_search_diagnostic restore set-failed
      return 1
    }
  elif ! security list-keychains -d user -s "${macos_keychain_search_original[@]}" >/dev/null 2>&1; then
    macos_keychain_search_diagnostic restore set-failed
    return 1
  fi
  macos_keychain_search_read restore || return
  if ! macos_keychain_search_matches original "$macos_keychain_search_original_count"; then
    macos_keychain_search_diagnostic restore readback-mismatch
    return 1
  fi
  macos_keychain_search_diagnostic restore ready
}

macos_keychain_search_restore() {
  local status=0
  if [[ $macos_keychain_search_restore_needed == 1 ]]; then
    macos_keychain_search_restore_list || status=1
  fi
  if [[ -n $macos_keychain_search_work ]]; then
    if ! rm -rf -- "$macos_keychain_search_work"; then
      macos_keychain_search_diagnostic restore private-state-cleanup-failed
      status=1
    fi
  fi
  macos_keychain_search_work=
  macos_keychain_search_restore_needed=0
  return "$status"
}
