#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
python_tool=${PYTHON:-python3}
"$python_tool" -m unittest discover -s "$root/tests" -p test_installer.py -v
if [[ $# == 0 ]]; then exit 0; fi
archive=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
ditto -x -k "$archive" "$work"
installer="$work/Agent Job Queue Installer.app"
codesign --verify --strict --deep "$installer"
"$python_tool" -B - "$installer" "$root" <<'PY'
import json
import plistlib
import subprocess
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[2])
from release_metadata import validate_build_info
app = Path(sys.argv[1])
payload = app / 'Contents/Resources/payload'
identity_bytes = (payload / 'source/build-info.json').read_bytes()
identity = validate_build_info(json.loads(identity_bytes))
for bundle, executable in ((app, 'Installer'), (payload / 'Agent Queue.app', 'AgentQueue')):
    info = plistlib.loads((bundle / 'Contents/Info.plist').read_bytes())
    assert info['LSMinimumSystemVersion'] == '15.0'
    assert info['CFBundleShortVersionString'] == identity['version']
    assert info['CFBundleVersion'] == str(identity['build'] or 0)
    assert (bundle / 'Contents/Resources/build-info.json').read_bytes() == identity_bytes
    architectures = subprocess.check_output(['xcrun', 'lipo', '-archs', str(bundle / 'Contents/MacOS' / executable)], text=True).split()
    assert set(architectures) == {'arm64', 'x86_64'}, architectures
for name in ('agent_heavy.py', 'agent_integration.py', 'resource_monitor.py', 'job_profiles.py', 'resource_scheduler.py',
             'app_updates.py', 'release_metadata.py', 'build-info.json',
             'remote/control.py', 'remote/server.py', 'remote/requirements.txt', 'remote/requirements.lock',
             'remote/static/index.html', 'remote/static/vendor/zod.mjs', 'bin/agent-heavy', 'bin/agent-remote',
             'scripts/install-user.sh', 'scripts/migrate-installation.py', 'LICENSE', 'VERSION'):
    assert (payload / 'source' / name).is_file(), name
for path in payload.rglob('*'):
    assert path.name not in {'.venv', '__pycache__', 'remote.json', 'state.json', 'root.crt', 'root.key', '.DS_Store'}
    assert not path.name.endswith('.pyc')
print('Universal installer payload, build identity, signatures and private-state exclusion verified.')
PY
