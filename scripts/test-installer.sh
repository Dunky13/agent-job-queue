#!/bin/bash
# Inspect the distributed app archive, then run its packaged helpers in an isolated private home.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
go test -p 2 -parallel 2 ./internal/backend -run '^TestInstaller' -count=1
if [[ $# == 0 ]]; then exit 0; fi
archive=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ditto -x -k "$archive" "$work"
app="$work/Agent Queue.app"
# The archive must contain only the app: nothing else is offered to the user.
shopt -s nullglob dotglob
extracted=("$work"/*)
[[ ${#extracted[@]} == 1 && ${extracted[0]} == "$app" ]] || {
  printf 'Archive must contain only Agent Queue.app.\n' >&2
  exit 1
}
go run -p 2 ./cmd/queue-tool verify-app --app "$app"
# Every native suite links the test build, so launch the real release binary here, once.
# It exits before setup. If that regressed, --fixture still blocks setup and helpers, this
# copy is outside Applications, and the empty home and environment hide the live queue.
refusal_home="$work/refusal-home"
mkdir "$refusal_home"
refusal_status=0
env -i HOME="$refusal_home" CFFIXED_USER_HOME="$refusal_home" /usr/bin/perl -e 'alarm 30; exec @ARGV' \
  "$app/Contents/MacOS/AgentQueue" --fixture private.json > "$work/refusal.log" 2>&1 || refusal_status=$?
if [[ $refusal_status != 1 ]] || ! grep -qF 'Test options need the test build' "$work/refusal.log" \
  || [[ -n "$(find "$refusal_home" -mindepth 1)" ]]; then
  printf 'The packaged app did not refuse --fixture cleanly: exit %s.\n' "$refusal_status" >&2
  cat "$work/refusal.log" >&2
  find "$refusal_home" -mindepth 1 >&2
  exit 1
fi
AGENT_QUEUE_TEST_APP_BUNDLE="$app" \
  go test -p 2 -parallel 2 ./internal/backend -run '^TestInstallerPackaged(AppBundle|LiveResume)$' -count=1 -v -timeout 3m
AGENT_QUEUE_TEST_APP_BUNDLE="$app" \
  go test -p 2 -parallel 2 ./internal/remote -run '^TestRemotePackagedGateway$' -count=1 -v -timeout 3m
