#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -gt 1 ]]; then
  printf 'Usage: %s [all|core|https|installer]\n' "$0" >&2
  exit 1
fi
scope=${1-all}
case "$scope" in
  all|core|https|installer) ;;
  *) printf 'Usage: %s [all|core|https|installer]\n' "$0" >&2; exit 1 ;;
esac

# Keep the original CI scope dynamic, and stop before testing on list errors.
packages=$(go list -p 2 ./cmd/... ./internal/... ./packaging/homebrew ./remote/static)
module=github.com/Dunky13/agent-job-queue
tooling="$module/internal/tooling"
https="$module/internal/remote"
backend="$module/internal/backend"
installer_test=TestInstallerManagedLiveUpgradeRetainsRunningAttempts
installer_pattern="^${installer_test}$"
runtime=()
seen=()
found_tooling=false
found_https=false
found_backend=false
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
  if [[ "$package" == "$backend" ]]; then found_backend=true; fi
  if [[ "$package" == "$tooling" ]]; then
    found_tooling=true
  elif [[ "$package" == "$https" ]]; then
    found_https=true
    if [[ "$scope" == all || "$scope" == https ]]; then
      runtime+=("$package")
    fi
  else
    if [[ "$scope" == all || "$scope" == core || ( "$scope" == installer && "$package" == "$backend" ) ]]; then
      runtime+=("$package")
    fi
  fi
done <<< "$packages"
if [[ "$found_tooling" != true || "$found_https" != true || "$found_backend" != true || ${#runtime[@]} -eq 0 ]]; then
  printf 'Go package enumeration requires tooling and HTTPS and backend packages and a nonempty runtime suite.\n' >&2
  exit 1
fi

if [[ "$scope" == core || "$scope" == installer ]]; then
  # Validate the exact race-built top-level inventory before either partial
  # backend selection. A renamed or missing test must never silently disappear.
  inventory=$(go test -race -p 2 ./internal/backend -list "$installer_pattern")
  found_test=false
  summary=false
  summary_detail='^([0-9]+([.][0-9]+)?s([[:space:]]+\(cached\))?|\(cached\))$'
  while IFS= read -r name || [[ -n "$name" ]]; do
    if [[ "$name" == "$installer_test" ]]; then
      if [[ "$found_test" == true || "$summary" == true ]]; then
        printf 'Duplicate or misplaced installer test inventory: %s\n' "$name" >&2
        exit 1
      fi
      found_test=true
    else
      read -r status listed detail <<< "$name"
      if [[ "$found_test" != true || "$summary" != false || "$status" != ok || "$listed" != "$backend" || ! "$detail" =~ $summary_detail ]]; then
        printf 'Unexpected installer test inventory record: %s\n' "$name" >&2
        exit 1
      fi
      summary=true
    fi
  done <<< "$inventory"
  if [[ "$found_test" != true || "$summary" != true ]]; then
    printf 'Installer test enumeration requires exactly one test and a complete summary.\n' >&2
    exit 1
  fi
fi

if [[ "$scope" == installer ]]; then
  go test -p 2 -parallel 2 -race "$backend" -run "$installer_pattern" -timeout 12m
  exit
fi
if [[ "$scope" == core ]]; then
  go test -p 2 -parallel 2 -race "$backend" -skip "$installer_pattern" -timeout 12m
  other=()
  for package in "${runtime[@]}"; do
    if [[ "$package" != "$backend" ]]; then other+=("$package"); fi
  done
  if [[ ${#other[@]} -gt 0 ]]; then
    go test -p 2 -parallel 2 -race "${other[@]}" -timeout 12m
  fi
  exit
fi
go test -p 2 -parallel 2 -race "${runtime[@]}" -timeout 12m
