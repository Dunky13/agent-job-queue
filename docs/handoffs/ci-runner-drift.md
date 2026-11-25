# CI runner drift: Homebrew cask path loading and OpenTelemetry advisory

From 2026-11-24 every branch failed hosted CI for two reasons outside the repository's code, and the tap's refresh workflow failed for the first of them. Example run: https://github.com/Dunky13/agent-job-queue/actions/runs/45633484606 (PR #87).

## Result

- **Homebrew:** Homebrew 7.0.13 added `Cask::CaskLoader.from_path` and deprecated loading cask files by path through `Cask::CaskLoader.load`; 7.0.14 rejects it. The Apple Silicon runners reached 7.0.14 around 2026-11-24, while the Intel runners still passed. `packaging/homebrew/test-cask.rb` now has `load_delivery_cask`, which uses `from_path` when Homebrew has it and `load` otherwise. The script and both tooling tests that load casks use it. Paths outside a tap pass Homebrew's tap trust check unchanged.
- **OpenTelemetry:** GO-2026-6974 is fixed in `go.opentelemetry.io/otel/sdk` v1.47.0, which requires `otel/log` v1.47.0. The indirect `contrib/exporters/autoexport` moves to v0.72.0 so the exporters stay on the release set built with it: OpenTelemetry v1.47.0, log exporters v0.23.0, and with them grpc v1.84.0.

## Evidence

- **Before:** hosted CI on Homebrew 7.0.14 (run above), and locally on `main` (a3336c2) with Homebrew 7.0.16-36, `TestHomebrewNativeAppLifecycle`, `TestHomebrewAppCaskPreservesQuarantineWithFixtureApp` and `TestManagedCaskLoaderRejectsHookDrift` fail with "Loading casks from paths, URLs or Ruby source is not supported". `govulncheck` reports GO-2026-6974 as called.
- **After:** the three tests pass, and the drift test still rejects every changed cask. `govulncheck` reports 0 affecting vulnerabilities. `go build ./...`, `go vet` on the tooling, remote and command packages, and the `internal/remote` tests pass. In the tap checkout, `brew ruby test-cask.rb Casks/agent-job-queue.rb` fails before and passes after, and `go test refresh.go refresh_test.go` passes.

## Next steps

1. Merge the companion tap PR copying `test-cask.rb` to `Dunky13/homebrew-agent-job-queue`, whose refresh workflow fails since 2026-11-24 ("Required brew operation failed"). Merge it before this PR, as `docs/homebrew-delivery.md` requires.
2. Rebase open PRs onto `main` after this merges.
