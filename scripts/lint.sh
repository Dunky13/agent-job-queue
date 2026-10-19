#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
python scripts/check-public-source.py
python -m ruff check .
npm run lint:web
shellcheck bin/agent-heavy bin/agent-remote .githooks/pre-push scripts/lint.sh scripts/test-native.sh scripts/capture-native-screenshots.sh
while IFS= read -r -d '' path; do
  zsh -n "$path"
done < <(git ls-files --cached --others --exclude-standard -z '*.sh')
actionlint .github/workflows/ci.yml packaging/homebrew/.github/workflows/refresh.yml
ruby -c packaging/homebrew/test-cask.rb
