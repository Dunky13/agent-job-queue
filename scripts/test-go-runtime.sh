#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ $# -gt 1 ]]; then
  printf 'Usage: %s [all|core|core-1|core-2|https|installer]\n' "$0" >&2
  exit 1
fi
scope=${1-all}
case "$scope" in
  all|core|core-1|core-2|https|installer) ;;
  *) printf 'Usage: %s [all|core|core-1|core-2|https|installer]\n' "$0" >&2; exit 1 ;;
esac

# Race instrumentation makes cost-14 bcrypt about ten times slower, which pushes
# one first authentication or password hash past the gateway's 30-second and the
# fixtures' 35-second budgets on slower hosts. Blowfish is sequential arithmetic
# on local state with no goroutines, so it alone is built without the detector;
# every other package, including the admission and gateway code, stays instrumented.
bcrypt_flags=(-gcflags='golang.org/x/crypto/blowfish=-race=false')

# Keep the original CI scope dynamic, and stop before testing on list errors.
packages=$(go list -p 2 ./cmd/... ./internal/... ./remote/static)
module=github.com/Dunky13/agent-job-queue
tooling="$module/internal/tooling"
https="$module/internal/remote"
backend="$module/internal/backend"
installer_test=TestInstallerManagedLiveResumeRetainsRunningAttempts
installer_pattern="^${installer_test}$"
summary_detail='^([0-9]+([.][0-9]+)?s([[:space:]]+\(cached\))?|\(cached\))$'
tab=$'\t'
backend_weights=${AGENT_QUEUE_BACKEND_WEIGHTS:-scripts/backend-test-weights.tsv}
runtime=()
seen=()
found_tooling=false
found_https=false
found_backend=false
while IFS= read -r package || [[ -n "$package" ]]; do
  case "$package" in
    "$module"/cmd|"$module"/cmd/*|"$module"/internal|"$module"/internal/*|"$module"/remote/static) ;;
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
  elif [[ "$scope" == all ]]; then
    runtime+=("$package")
  elif [[ "$package" == "$backend" ]]; then
    if [[ "$scope" != https ]]; then runtime+=("$package"); fi
  elif [[ "$scope" == https ]]; then
    # The other runtime packages share the HTTPS job's race-built caddy dependencies.
    runtime+=("$package")
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
if [[ "$scope" == core || "$scope" == core-1 || "$scope" == core-2 ]]; then
  if [[ "$scope" == core ]]; then
    go test -p 2 -parallel 2 -race "$backend" -skip "$installer_pattern" -timeout 12m
  else
    # Enumerate before selecting so every ordinary test, example and fuzz seed
    # runs exactly once. The live-resume test retains its independent job.
    inventory=$(go test -race -p 2 ./internal/backend -list .)
    tests=()
    listed_tests=()
    summary=false
    found_installer=false
    while IFS= read -r name || [[ -n "$name" ]]; do
      if [[ $name =~ ^(Test|Example|Fuzz|Benchmark)[a-zA-Z0-9_]*$ ]]; then
        [[ $summary == false ]] || { printf 'Test after backend inventory summary.\n' >&2; exit 1; }
        for previous in ${listed_tests[@]+"${listed_tests[@]}"}; do
          [[ $previous != "$name" ]] || { printf 'Duplicate backend inventory test: %s\n' "$name" >&2; exit 1; }
        done
        listed_tests+=("$name")
        case "$name" in
          "$installer_test") found_installer=true ;;
          Benchmark*) ;;
          *) tests+=("$name") ;;
        esac
      else
        read -r status listed detail <<< "$name"
        if [[ $summary != false || $status != ok || $listed != "$backend" || ! $detail =~ $summary_detail ]]; then
          printf 'Unexpected backend inventory record: %s\n' "$name" >&2
          exit 1
        fi
        summary=true
      fi
    done <<< "$inventory"
    [[ $summary == true && $found_installer == true && ${#tests[@]} -ge 2 ]] || {
      printf 'Backend inventory requires a complete installer and nonempty two-shard suite.\n' >&2
      exit 1
    }
    shard=${scope#core-}
    # Balance measured durations rather than alternate names, so one slow or newly
    # added test cannot move half the suite. Unmeasured tests count one second.
    weighted=$(printf '%s\n' "${tests[@]}" | LC_ALL=C awk -v weights="$backend_weights" '
      BEGIN {
        FS = "\t"
        while ((status = (getline line < weights)) > 0) {
          if (line == "" || line ~ /^#/) continue
          if (split(line, field, "\t") != 2 || field[1] !~ /^(Test|Example|Fuzz)[a-zA-Z0-9_]*$/ || field[2] !~ /^[1-9][0-9]*$/ || field[1] in weight) {
            print "Invalid backend weight record: " line > "/dev/stderr"; failed = 1; exit 2
          }
          weight[field[1]] = field[2]
        }
        if (status < 0) { print "Backend weights are unreadable: " weights > "/dev/stderr"; failed = 1; exit 2 }
      }
      { listed[$0] = 1; print ($0 in weight ? weight[$0] : 1000) "\t" $0 "\t" ($0 in weight ? "" : "unweighted") }
      END {
        # exit in BEGIN still runs END.
        if (failed) exit 2
        for (name in weight) if (!(name in listed)) stale++
        printf "Backend weights: %d recorded tests are no longer listed\n", stale > "/dev/stderr"
      }' |
      LC_ALL=C sort -t "$tab" -k1,1nr -k2,2 |
      LC_ALL=C awk -F '\t' '{ if (first <= second) { first += $1; part = 1 } else { second += $1; part = 2 }; print part "\t" $2 "\t" $3 }
        END { printf "sums\t%d\t%d\n", first, second }')
    selected=()
    sums=()
    unweighted=0
    # Records are "shard, test, unweighted marker", then "sums, first, second".
    while IFS="$tab" read -r first second third; do
      if [[ "$first" == sums ]]; then
        sums=("$second" "$third")
      elif [[ "$first" == "$shard" ]]; then
        selected+=("$second")
        if [[ "$third" == unweighted ]]; then unweighted=$((unweighted + 1)); fi
      fi
    done <<< "$weighted"
    [[ ${#selected[@]} -gt 0 && ${#sums[@]} -eq 2 ]] || { printf 'Empty backend inventory shard.\n' >&2; exit 1; }
    printf 'Backend shard %s/2: %s of %s tests, examples and fuzz seeds\n' "$shard" "${#selected[@]}" "${#tests[@]}"
    printf 'Backend shard weights: %s ms and %s ms; %s selected tests unweighted\n' "${sums[0]}" "${sums[1]}" "$unweighted"
    pattern=$(printf '%s\n' "${selected[@]}" | LC_ALL=C sort | paste -sd '|' -)
    go test -p 2 -parallel 2 -race -v "$backend" -run "^($pattern)$" -timeout 12m
  fi
  exit
fi
if [[ "$scope" == https ]]; then
  # The gateway runs alone first: its admission budgets assume no competing test binary.
  go test -p 2 -parallel 2 -race "${bcrypt_flags[@]}" "$https" -timeout 12m
  other=()
  for package in "${runtime[@]}"; do
    if [[ "$package" != "$https" ]]; then other+=("$package"); fi
  done
  if [[ ${#other[@]} -gt 0 ]]; then
    go test -p 2 -parallel 2 -race "${bcrypt_flags[@]}" "${other[@]}" -timeout 12m
  fi
  exit
fi
go test -p 2 -parallel 2 -race "${bcrypt_flags[@]}" "${runtime[@]}" -timeout 12m
