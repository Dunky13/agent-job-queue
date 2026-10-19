#!/bin/bash
# Capture actual native views with inert, public sample jobs.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
output="$root/docs/screenshots"
mkdir -p "$output"
# Keep elapsed labels readable when regenerating these screenshots later.
python3 - "$output/native-fixture.json" "$temporary/fixture.json" <<'PY'
from datetime import datetime, timezone
import json
from pathlib import Path
import sys

data = json.loads(Path(sys.argv[1]).read_text())
offset = datetime.now(timezone.utc) - datetime.fromisoformat(data['updatedAt'])
for job in data['jobs']:
    for key in ('submittedAt', 'startedAt', 'finishedAt'):
        if job[key] is not None:
            job[key] = (datetime.fromisoformat(job[key]) + offset).isoformat()
data['updatedAt'] = datetime.now(timezone.utc).isoformat()
Path(sys.argv[2]).write_text(json.dumps(data))
PY
ARCHS=$(uname -m) "$root/native/build.sh" "$temporary/AgentQueue"
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$output/mac-queue.png" --snapshot-menu
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$temporary/confirmation.png" --snapshot-menu --snapshot-confirmation start \
  --verify-confirmation-navigation "$temporary/navigation.json"
cp "$temporary/confirmation.png" "$output/mac-start-now.png"
"$temporary/AgentQueue" --fixture "$temporary/fixture.json" \
  --snapshot "$output/agent-integration.png" --snapshot-agent-integration
