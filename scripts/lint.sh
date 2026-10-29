#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
go run -p 2 ./cmd/queue-tool check-source
unformatted=$(gofmt -l cmd internal remote/static/embed.go packaging/homebrew/refresh.go packaging/homebrew/refresh_test.go)
[[ -z "$unformatted" ]] || { printf 'Go files need formatting:\n%s\n' "$unformatted" >&2; exit 1; }
go vet -p 2 ./cmd/... ./internal/... ./packaging/homebrew ./remote/static
npm run lint:web
shellcheck bin/agent-heavy bin/agent-remote .githooks/pre-push
while IFS= read -r -d '' path; do
  [[ -f "$path" ]] || continue
  case "$(head -n 1 "$path")" in
    '#!/bin/zsh'|'#!/usr/bin/env zsh') ;;
    *) shellcheck -x "$path" ;;
  esac
  zsh -n "$path"
done < <(git ls-files --cached --others --exclude-standard -z '*.sh')
actionlint .github/workflows/*.yml packaging/homebrew/.github/workflows/refresh.yml
ruby -c packaging/homebrew/test-cask.rb
