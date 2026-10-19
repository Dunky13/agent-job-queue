#!/bin/bash
# Per-user installation. Existing queue data/configuration and running jobs survive upgrades.
set -euo pipefail
umask 077
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
locked=0
if [[ ${1:-} == --locked ]]; then locked=1; shift; fi
payload=${2:-}
if [[ ${1:-} != --payload || -z "$payload" || $# != 2 ]]; then
  echo 'Usage: install-user.sh --payload DIRECTORY' >&2
  exit 2
fi
if [[ ${AGENT_QUEUE_INSTALL_TEST_MODE:-} == 1 ]]; then
  export PATH="${AGENT_QUEUE_INSTALL_TEST_TOOLS:?}:$PATH"
else
  [[ $(uname -s) == Darwin ]] || { echo 'Agent Job Queue requires macOS.' >&2; exit 1; }
fi
[[ -d "$payload/Agent Queue.app" && -f "$payload/source/agent_heavy.py" && -f "$payload/source/agent_integration.py" && -f "$payload/source/remote/requirements.txt" ]] || {
  echo 'Incomplete installer payload.' >&2; exit 1;
}
brew_tool=$(command -v brew || true)
[[ -n "$brew_tool" ]] || { echo 'Homebrew is required. Install Homebrew from https://brew.sh, then run this installer again.' >&2; exit 1; }
root="$HOME/.local/share/agent-heavy"
state="$HOME/.local/state/agent-heavy"
config="$HOME/.config/agent-heavy"
mkdir -p "$root" "$state" "$config" "$HOME/.local/bin" "$HOME/Applications" "$HOME/Library/LaunchAgents"
for directory in "$root" "$state" "$config"; do
  [[ -d "$directory" && -O "$directory" && ! -L "$directory" ]] || {
    echo 'Installer directories must be owned by this account and must not be symbolic links.' >&2
    exit 1
  }
  chmod 700 "$directory"
done
# Kernel locking releases automatically after crashes and never breaks a live owner's lock.
lock="$state/install.lock"
[[ ! -L "$lock" && ( ! -e "$lock" || ( -f "$lock" && -O "$lock" ) ) ]] || {
  echo 'Unsafe installer lock ownership or file type.' >&2; exit 1;
}
if [[ "$locked" == 0 ]]; then
  set +e
  /usr/bin/lockf -k -s -t 0 "$lock" /bin/bash "$0" --locked --payload "$payload"
  status=$?
  if [[ "$status" == 75 ]]; then
    echo 'Another installer is running. Finish it before trying again.' >&2
  fi
  exit "$status"
fi
printf 'Checking Homebrew prerequisites...\n'
# Do not upgrade an existing Pueue daemon binary while it owns running work.
missing=()
command -v pueue >/dev/null && command -v pueued >/dev/null || missing+=(pueue)
command -v caddy >/dev/null || missing+=(caddy)
command -v uv >/dev/null || missing+=(uv)
python_prefix=$("$brew_tool" --prefix python@3.12 2>/dev/null || true)
[[ -n "$python_prefix" && -x "$python_prefix/bin/python3.12" ]] || missing+=(python@3.12)
if (( ${#missing[@]} )); then
  HOMEBREW_NO_AUTO_UPDATE=1 "$brew_tool" install "${missing[@]}"
fi
python_prefix=$("$brew_tool" --prefix python@3.12)
python_tool="$python_prefix/bin/python3.12"
[[ -x "$python_tool" ]] || { echo 'Homebrew Python 3.12 is unavailable.' >&2; exit 1; }
pueued_tool=$(command -v pueued)
pueue_tool=$(command -v pueue)
"$python_tool" "$payload/source/scripts/migrate-installation.py" "$root" "$state"
printf 'Installing application support files...\n'
# Copy only the public payload. Runtime state, credentials and the existing venv stay private.
cp "$payload/source/agent_heavy.py" "$root/agent_heavy.py"
cp "$payload/source/agent_integration.py" "$root/agent_integration.py"
mkdir -p "$root/remote/static" "$root/bin" "$root/scripts"
cp "$payload/source/remote/control.py" "$payload/source/remote/server.py" "$payload/source/remote/requirements.txt" "$payload/source/remote/requirements.lock" "$root/remote/"
cp -R "$payload/source/remote/static/." "$root/remote/static/"
cp "$payload/source/bin/agent-heavy" "$payload/source/bin/agent-remote" "$root/bin/"
cp "$payload/source/install-remote.sh" "$root/install-remote.sh"
cp "$payload/source/scripts/install-user.sh" "$payload/source/scripts/migrate-installation.py" "$root/scripts/"
for document in README.md LICENSE THIRD-PARTY-NOTICES.md VERSION; do
  [[ ! -f "$payload/source/$document" ]] || cp "$payload/source/$document" "$root/$document"
done
if [[ ! -x "$root/.venv/bin/python" ]]; then
  uv venv --python "$python_tool" "$root/.venv"
fi
uv pip install --python "$root/.venv/bin/python" --require-hashes -r "$root/remote/requirements.lock"
install -m 755 "$root/bin/agent-heavy" "$HOME/.local/bin/agent-heavy"
install -m 755 "$root/bin/agent-remote" "$HOME/.local/bin/agent-remote"
fresh_config=0
[[ -f "$config/pueue.yml" ]] || fresh_config=1
printf 'Preparing private queue and login entries...\n'
"$root/.venv/bin/python" - "$root" "$state" "$config" "$pueued_tool" <<'PY'
import json
import os
import plistlib
import shlex
import sys
from pathlib import Path
root, state, config, daemon = map(Path, sys.argv[1:])
home = Path.home()
for directory in (state, state / 'pueue', state / 'run', config):
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    directory.chmod(0o700)
queue_config = config / 'pueue.yml'
if not queue_config.exists():
    data = {
        'shared': {'pueue_directory': str(state / 'pueue'), 'runtime_directory': str(state / 'run'),
                   'use_unix_socket': True, 'unix_socket_permissions': 0o600},
        'daemon': {'pause_group_on_failure': False, 'pause_all_on_failure': False,
                   'compress_state_file': False,
                   'callback': shlex.join([str(root / '.venv/bin/python'), str(root / 'agent_heavy.py'), 'cleanup']) + ' {{ id }}',
                   'shell_command': ['/bin/sh', '-c', '{{ pueue_command_string }}']},
        'client': {'show_confirmation_questions': False, 'read_local_logs': True, 'edit_mode': 'files'},
    }
    with queue_config.open('x') as handle:
        json.dump(data, handle, indent=2)
    queue_config.chmod(0o600)
launch = home / 'Library/LaunchAgents'
queue_plist = launch / 'io.went.agent-heavy.plist'
# An existing service may be running actual work. Leave its definition and process alone.
if not queue_plist.exists():
    queue_plist.write_bytes(plistlib.dumps({
        'Label': 'io.went.agent-heavy', 'ProgramArguments': [str(daemon), '--config', str(queue_config)],
        'EnvironmentVariables': {'HOME': str(home), 'PATH': '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin',
                                 **{key: os.environ[key] for key in ('AGENT_HEAVY_PUEUE', 'AGENT_HEAVY_LEGACY_SLOT') if key in os.environ}},
        'KeepAlive': {'SuccessfulExit': False}, 'RunAtLoad': True,
        'StandardErrorPath': str(state / 'daemon.log'), 'StandardOutPath': str(state / 'daemon.log'),
        'ThrottleInterval': 10, 'Umask': 0o077, 'WorkingDirectory': str(state),
    }))
    queue_plist.chmod(0o600)
app = home / 'Applications/Agent Queue.app'
menu_plist = launch / 'io.went.agent-queue.plist'
menu_plist.write_bytes(plistlib.dumps({'Label': 'io.went.agent-queue',
    'ProgramArguments': ['/usr/bin/open', '-a', str(app)], 'RunAtLoad': True, 'KeepAlive': False}))
menu_plist.chmod(0o600)
PY
service="gui/$(id -u)/io.went.agent-heavy"
if launchctl print "$service" >/dev/null 2>&1; then
  printf 'Keeping the running queue daemon and its jobs.\n'
else
  launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/io.went.agent-heavy.plist"
fi
ready=0
for _attempt in {1..30}; do
  if "$pueue_tool" --config "$config/pueue.yml" --color never status --json >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" == 1 ]] || { echo 'The queue daemon did not become ready. Inspect the private daemon log.' >&2; exit 1; }
if [[ "$fresh_config" == 1 ]]; then
  "$pueue_tool" --config "$config/pueue.yml" --color never parallel 1 --group default
fi
printf 'Installing the prebuilt menu-bar app...\n'
app="$HOME/Applications/Agent Queue.app"
staging="$HOME/Applications/.Agent Queue.installing.app"
rm -rf "$staging"
cp -R "$payload/Agent Queue.app" "$staging"
# Quit only the menu-bar UI. Its daemon-owned jobs and optional remote service keep running.
if [[ ${AGENT_QUEUE_INSTALL_TEST_MODE:-} != 1 ]]; then
  /usr/bin/osascript -e 'tell application id "io.went.agent-queue" to quit' >/dev/null 2>&1 || true
fi
rm -rf "$app"
mv "$staging" "$app"
if [[ ${AGENT_QUEUE_INSTALL_TEST_MODE:-} != 1 ]]; then
  /usr/bin/open -a "$app"
fi
printf 'Installed Agent Queue. The queue remains private; phone access is off until enabled in settings.\n'
printf 'CLI: %s/.local/bin/agent-heavy\n' "$HOME"
printf 'Add your local bin directory to your shell PATH to use agent-heavy by name.\n'
