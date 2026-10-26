#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -gt 1 ]]; then
  printf 'Usage: %s [all|core|https]\n' "$0" >&2
  exit 1
fi
scope=${1-all}
case "$scope" in
  all|core|https) ;;
  *) printf 'Usage: %s [all|core|https]\n' "$0" >&2; exit 1 ;;
esac

# Keep the original CI scope dynamic, and stop before testing on list errors.
packages=$(go list -p 2 ./cmd/... ./internal/... ./packaging/homebrew ./remote/static)
module=github.com/Dunky13/agent-job-queue
tooling="$module/internal/tooling"
https="$module/internal/remote"
runtime=()
seen=()
found_tooling=false
found_https=false
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
  elif [[ "$package" == "$https" ]]; then
    found_https=true
    if [[ "$scope" != core ]]; then
      runtime+=("$package")
    fi
  else
    if [[ "$scope" != https ]]; then
      runtime+=("$package")
    fi
  fi
done <<< "$packages"
if [[ "$found_tooling" != true || "$found_https" != true || ${#runtime[@]} -eq 0 ]]; then
  printf 'Go package enumeration requires tooling and HTTPS packages and a nonempty runtime suite.\n' >&2
  exit 1
fi
go test -p 2 -parallel 2 -race "${runtime[@]}" -timeout 12m
