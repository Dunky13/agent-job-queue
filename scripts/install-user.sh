#!/bin/bash
# The packaged Go controller owns installation and migration checks.
set -euo pipefail
if [[ $# != 2 || $1 != --payload || -z $2 ]]; then
  echo 'Usage: install-user.sh --payload DIRECTORY' >&2
  exit 2
fi
payload=$2
[[ -x "$payload/source/bin/agent-heavy" ]] || {
  echo 'Incomplete installer payload: Go controller is missing.' >&2
  exit 1
}
exec "$payload/source/bin/agent-heavy" install --payload "$payload"
