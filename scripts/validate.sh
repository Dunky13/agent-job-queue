#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
bash scripts/lint.sh
go run -p 2 golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./cmd/agent-heavy ./cmd/agent-remote ./cmd/queue-tool
go test -p 2 -parallel 2 -race ./cmd/... ./internal/... ./packaging/homebrew ./remote/static -timeout 12m
bash scripts/build-go.sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -o build/queue-preview-linux ./cmd/queue-preview
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -o build/queue-tool-linux ./cmd/queue-tool
npm run test:web
npm run test:browser
bash scripts/test-native.sh
scripts/build-installer.sh
scripts/test-installer.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.zip"
bash scripts/test-dmg.sh "dist/Agent-Job-Queue-Installer-$(cat VERSION)-universal.dmg"
