#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $# == 1 && ( $1 == 1 || $1 == 2 ) ]] || {
  printf 'Usage: test-go-tooling.sh SHARD (1 or 2)\n' >&2
  exit 1
}
shard=$1
package=github.com/Dunky13/agent-job-queue/internal/tooling
# Discover every top-level test, example and fuzz seed. Enumeration failures or
# malformed/duplicate records must stop before any partial suite is executed.
inventory=$(go test -p 2 ./internal/tooling -list .)
tests=()
seen=()
summary=false
while IFS= read -r name || [[ -n "$name" ]]; do
  if [[ $name =~ ^(Test|Example|Fuzz|Benchmark)[a-zA-Z0-9_]*$ ]]; then
    [[ $summary == false ]] || { printf 'Test after enumeration summary.\n' >&2; exit 1; }
    for previous in ${seen[@]+"${seen[@]}"}; do
      [[ $previous != "$name" ]] || { printf 'Duplicate Go test: %s\n' "$name" >&2; exit 1; }
    done
    seen+=("$name")
    case "$name" in Benchmark*) ;; *) tests+=("$name");; esac
  else
    read -r status listed detail <<< "$name"
    if [[ $summary != false || $status != ok || $listed != "$package" || -z $detail ]]; then
      printf 'Unexpected Go test inventory record: %s\n' "$name" >&2
      exit 1
    fi
    summary=true
  fi
done <<< "$inventory"
[[ $summary == true && ${#tests[@]} -ge 2 ]] || {
  printf 'Tooling enumeration requires a complete nonempty two-shard suite.\n' >&2
  exit 1
}
sorted=$(printf '%s\n' "${tests[@]}" | LC_ALL=C sort)
selected=()
index=0
while IFS= read -r name; do
  if (( index % 2 + 1 == shard )); then selected+=("$name"); fi
  index=$((index + 1))
done <<< "$sorted"
[[ ${#selected[@]} -gt 0 ]] || { printf 'Empty tooling shard.\n' >&2; exit 1; }
printf 'Tooling shard %s/2: %s of %s top-level tests\n' "$shard" "${#selected[@]}" "${#tests[@]}"
printf '  %s\n' "${selected[@]}"
pattern=$(IFS='|'; printf '%s' "${selected[*]}")
go test -p 2 -parallel 2 -race -v ./internal/tooling -run "^($pattern)$" -timeout 12m
