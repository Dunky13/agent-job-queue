#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
bash scripts/lint.sh
go run -p 2 golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./cmd/agent-heavy ./cmd/agent-remote ./cmd/queue-tool
# Run the partitions hosted CI requires. -timeout applies per package, so one go test over
# internal/backend and internal/tooling cannot fit in 12 minutes. The drivers keep -race, two
# workers and that budget per partition, and together cover every Go package exactly once.
bash scripts/test-go-runtime.sh core-1
bash scripts/test-go-runtime.sh core-2
bash scripts/test-go-runtime.sh https
bash scripts/test-go-runtime.sh installer
bash scripts/test-go-tooling.sh 1
bash scripts/test-go-tooling.sh 2
bash scripts/build-go.sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -o build/queue-preview-linux ./cmd/queue-preview
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -p 2 -trimpath -o build/queue-tool-linux ./cmd/queue-tool
npm run test:web
npm run test:browser
bash scripts/test-native.sh
scripts/build-app.sh
scripts/test-installer.sh "dist/Agent-Queue-$(cat VERSION)-universal.zip"
bash scripts/test-dmg.sh "dist/Agent-Queue-$(cat VERSION)-universal.dmg"
