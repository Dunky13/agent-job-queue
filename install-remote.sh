#!/bin/bash
# Optional dependency repair. Does not enable network access or alter credentials.
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
root=$(cd -- "$(dirname -- "$0")" && pwd)
brew_tool=$(command -v brew || true)
[[ -n "$brew_tool" ]] || { echo 'Homebrew is required: https://brew.sh' >&2; exit 1; }
missing=()
command -v caddy >/dev/null || missing+=(caddy)
command -v uv >/dev/null || missing+=(uv)
python_prefix=$("$brew_tool" --prefix python@3.12 2>/dev/null || true)
[[ -n "$python_prefix" && -x "$python_prefix/bin/python3.12" ]] || missing+=(python@3.12)
if (( ${#missing[@]} )); then HOMEBREW_NO_AUTO_UPDATE=1 "$brew_tool" install "${missing[@]}"; fi
python_prefix=$("$brew_tool" --prefix python@3.12)
[[ -x "$root/.venv/bin/python" ]] || uv venv --python "$python_prefix/bin/python3.12" "$root/.venv"
uv pip install --python "$root/.venv/bin/python" --require-hashes -r "$root/remote/requirements.lock"
install -m 755 "$root/bin/agent-remote" "$HOME/.local/bin/agent-remote"
printf 'Remote-access support installed. Enable it in Agent Queue > Remote access.\n'
