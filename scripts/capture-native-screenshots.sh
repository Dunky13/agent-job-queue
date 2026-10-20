#!/bin/bash
# Capture actual native views with inert, public sample jobs.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
output="$root/docs/screenshots"
mkdir -p "$output"
# Keep elapsed labels readable when regenerating these screenshots later.
(cd "$root" && go run -p 2 ./cmd/queue-tool shift-fixture --input "$output/native-fixture.json" --output "$temporary/fixture.json")
ARCHS=$(uname -m) "$root/native/build.sh" "$temporary/AgentQueue"
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$output/mac-queue.png" --snapshot-menu
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$temporary/confirmation.png" --snapshot-menu --snapshot-confirmation start \
  --verify-confirmation-navigation "$temporary/navigation.json"
cp "$temporary/confirmation.png" "$output/mac-start-now.png"
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$output/agent-integration.png" --snapshot-agent-integration
