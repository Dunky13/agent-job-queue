#!/bin/bash
# Build developer commands without modifying the installed private queue.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
mkdir -p build
for command in agent-heavy agent-remote queue-build-info queue-preview queue-tool queue-delivery; do
  go build -p 2 -trimpath -o "build/$command" "./cmd/$command"
done
