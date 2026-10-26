#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Keep the original CI scope dynamic, and stop before testing on list errors.
packages=$(go list -p 2 ./cmd/... ./internal/... ./packaging/homebrew ./remote/static)
module=github.com/Dunky13/agent-job-queue
tooling="$module/internal/tooling"
runtime=()
seen=()
found_tooling=false
while IFS= read -r package || [[ -n "$package" ]]; do
  case "$package" in
    "$module"/cmd|"$module"/cmd/*|"$module"/internal|"$module"/internal/*|"$module"/packaging/homebrew|"$module"/remote/static) ;;
    *) printf 'Unexpected Go package: %s\n' "$package" >&2; exit 1 ;;
  esac
  case "$package" in
    *[!a-zA-Z0-9_./-]*|*/) printf 'Unsafe Go package: %s\n' "$package" >&2; exit 1 ;;
  esac
  case "/$package/" in
    *'//'*|*'/./'*|*'/../'*|*'...'*) printf 'Unsafe Go package: %s\n' "$package" >&2; exit 1 ;;
  esac
  # Bash 3.2 on macOS has no associative arrays. The enumerated set is small.
  for previous in ${seen[@]+"${seen[@]}"}; do
    if [[ "$previous" == "$package" ]]; then
      printf 'Duplicate Go package: %s\n' "$package" >&2
      exit 1
    fi
  done
  seen+=("$package")
  if [[ "$package" == "$tooling" ]]; then
    found_tooling=true
  else
    runtime+=("$package")
  fi
done <<< "$packages"
if [[ "$found_tooling" != true || ${#runtime[@]} -eq 0 ]]; then
  printf 'Go package enumeration requires tooling and a nonempty runtime suite.\n' >&2
  exit 1
fi
go test -p 2 -parallel 2 -race "${runtime[@]}" -timeout 12m
