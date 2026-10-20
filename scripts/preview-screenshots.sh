#!/bin/bash
# Serve the actual dashboard with public simulated jobs and no installed queue.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ ! -x build/queue-preview ]]; then
  printf '%s\n' 'Build the preview first: go build -o build/queue-preview ./cmd/queue-preview' >&2
  exit 1
fi
exec build/queue-preview --screenshots "$@"
