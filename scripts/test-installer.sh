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
AGENT_QUEUE_TEST_APP_BUNDLE="$app" \
  go test -p 2 -parallel 2 ./internal/backend -run '^TestInstallerPackaged(AppBundle|LiveUpgrade)$' -count=1 -v -timeout 3m
AGENT_QUEUE_TEST_APP_BUNDLE="$app" \
  go test -p 2 -parallel 2 ./internal/remote -run '^TestRemotePackagedGateway$' -count=1 -v -timeout 3m
