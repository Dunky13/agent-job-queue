#!/bin/bash
# Finite local equivalent of CI. Apply the caller's heavy-command admission policy once.
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
[[ -x .venv/bin/python ]] || { echo 'Create the development venv first; see CONTRIBUTING.md.' >&2; exit 1; }
export PATH="$root/.venv/bin:$PATH"
export AGENT_QUEUE_TEST_PYTHON="$root/.venv/bin/python"
bash scripts/lint.sh
python -m unittest discover -s tests -p test_agent_heavy.py -v
python -m unittest discover -s tests -p 'test_resource*.py' -v
python -m unittest discover -s tests -p test_job_profiles.py -v
python -m unittest discover -s tests -p test_release_metadata.py -v
python -m unittest discover -s tests -p test_updates.py -v
python -m unittest discover -s tests -p test_agent_integration.py -v
python -m unittest discover -s tests -p test_integration.py -v
python -m unittest discover -s remote/tests -p test_remote.py -v
npm run test:web
npm run test:browser
bash scripts/test-native.sh
scripts/build-installer.sh
scripts/test-installer.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.zip"
