#!/bin/bash
# Inspect the distributed artifact, then install it into an isolated private home.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
go test -p 2 -parallel 2 ./internal/backend -run '^TestInstaller' -count=1
if [[ $# == 0 ]]; then exit 0; fi
archive=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ditto -x -k "$archive" "$work"
installer="$work/Agent Job Queue Installer.app"
go run -p 2 ./cmd/queue-tool verify-installer --installer "$installer"
AGENT_QUEUE_TEST_INSTALLER_PAYLOAD="$installer/Contents/Resources/payload" \
  go test -p 2 -parallel 2 ./internal/backend -run '^TestInstallerPackaged(Payload|AppBundle|LiveUpgrade)$' -count=1 -v -timeout 3m
AGENT_QUEUE_TEST_INSTALLER_PAYLOAD="$installer/Contents/Resources/payload" \
  go test -p 2 -parallel 2 ./internal/remote -run '^TestRemotePackagedGateway$' -count=1 -v -timeout 3m
AGENT_QUEUE_TEST_INSTALLER_PAYLOAD="$installer/Contents/Resources/payload" \
  go test -race -p 2 -parallel 2 ./internal/tooling -run '^TestManagedInstallerPackagedNativeFixture$' -count=1 -v -timeout 3m
